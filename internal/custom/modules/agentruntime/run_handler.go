package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/custom/modules/usererrors"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

type controlRequest struct {
	IncludeEvidence      bool            `json:"include_evidence"`
	OutputBaseline       json.RawMessage `json:"output_baseline"`
	Finalization         bool            `json:"finalization"`
	ModelRole            string          `json:"model_role"`
	EstimatedInputTokens int64           `json:"estimated_input_tokens"`
	MaxOutputTokens      int64           `json:"max_output_tokens"`
	RunID                string          `json:"run_id"`
	OwnerEpoch           int64           `json:"owner_epoch"`
	WorkerID             string          `json:"worker_id"`
	Checkpoint           json.RawMessage `json:"checkpoint"`
	Events               []StreamEvent   `json:"events"`
	Result               ChatResult      `json:"result"`
	Error                string          `json:"error"`
	ErrorCode            string          `json:"error_code"`
}

func controlFailure(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, errRunFenced) {
		status = http.StatusConflict
	}
	c.JSON(status, gin.H{"error": err.Error()})
}
func (h *Handler) RunControl(c *gin.Context) {
	if !validInternalAPIKey(c) {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<20)
	var req controlRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "invalid run control request"})
		return
	}
	ctx := c.Request.Context()
	op := c.Param("operation")
	if op == "budget" {
		var budget BudgetState
		err := h.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			row, err := lockRun(tx, req.RunID)
			if err != nil {
				return err
			}
			if err = owned(row, req.OwnerEpoch); err != nil {
				return err
			}
			budget, err = modelBudget(tx, row, req.ModelRole)
			if err == nil && req.IncludeEvidence {
				budget.CitationEvidence = sourcerefs.StructuredEvidence(row.References)
			}
			return err
		})
		if err != nil {
			controlFailure(c, err)
			return
		}
		c.JSON(200, budget)
		return
	}
	if op == "status" {
		var row RunRecord
		if err := h.service.db.WithContext(ctx).Select("id", "status", "owner_epoch").First(&row, "id = ?", req.RunID).Error; err != nil {
			controlFailure(c, err)
			return
		}
		c.JSON(200, gin.H{"status": row.Status, "owner_epoch": row.OwnerEpoch})
		return
	}
	if op == "claim" {
		if strings.TrimSpace(req.WorkerID) == "" {
			c.JSON(400, gin.H{"error": "worker_id required"})
			return
		}
		p, err := h.service.claimRun(ctx, req.WorkerID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(200, gin.H{"run": nil})
			return
		}
		if err != nil {
			controlFailure(c, err)
			return
		}
		c.JSON(200, gin.H{"run": p})
		return
	}
	if op == "checkpoint" {
		if err := h.service.saveCheckpoint(ctx, req.RunID, req.OwnerEpoch, req.Checkpoint); err != nil {
			controlFailure(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
		return
	}
	err := h.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := lockRun(tx, req.RunID)
		if err != nil {
			return err
		}
		// Completion is idempotent after a lost acknowledgement, but never permits
		// a different worker or candidate to replace an already committed result.
		if op == "commit" && (row.Status == "completed" || row.Status == "incomplete") && row.OwnerEpoch == req.OwnerEpoch {
			var prior ChatResult
			if err = json.Unmarshal(row.Result, &prior); err != nil {
				return err
			}
			replayed, _, _ := sourcerefs.RenderStructuredCitations(req.Result.Answer, req.Result.Citations, row.References)
			if replayed != prior.Answer {
				return errRunFenced
			}
			req.Result = prior
			return nil
		}
		if op == "finalize" {
			if row.OwnerEpoch != req.OwnerEpoch || req.OwnerEpoch < 1 || !row.LeaseUntil.After(time.Now()) {
				return errRunFenced
			}
			if row.Status == "finalizing" {
				return nil
			}
			if row.Status != "running" {
				return errRunFenced
			}
			return beginFinalization(tx, row, Finalization{Result: req.Result, Error: req.Error, ErrorCode: req.ErrorCode})
		}
		if row.Status == "finalizing" && (op == "heartbeat" || op == "commit" || op == "validate" || op == "fail") {
			err = ownedDelivery(row, req.OwnerEpoch)
		} else {
			err = owned(row, req.OwnerEpoch)
		}
		if err != nil {
			return err
		}
		switch op {
		case "baseline":
			var baseline map[string]string
			if json.Unmarshal(req.OutputBaseline, &baseline) != nil || baseline == nil {
				return fmt.Errorf("invalid output baseline")
			}
			if len(row.OutputBaseline) == 0 {
				row.OutputBaseline = req.OutputBaseline
			}
			req.OutputBaseline = row.OutputBaseline
		case "heartbeat":
			row.LeaseUntil = time.Now().Add(runLease)
		case "fail":
			return terminateRun(tx, row, "failed", req.Error, req.ErrorCode)
		case "events":
			for _, item := range req.Events {
				if item.Seq <= row.ClientSeq {
					continue
				}
				if item.Seq != row.ClientSeq+1 {
					return fmt.Errorf("non-contiguous runtime event sequence")
				}
				if err = outbox(tx, row, item); err != nil {
					return err
				}
				if item.Type == "local_tool_start" || item.Type == "local_tool_result" {
					if err = recordLocalTool(tx, row, item); err != nil {
						return err
					}
				}
				row.ClientSeq = item.Seq
				if item.Revision > row.Revision {
					row.Revision = item.Revision
				}
			}
		case "validate", "commit":
			violations, err := h.service.validateResult(ctx, tx, row, &req.Result)
			if err != nil {
				return err
			}
			if len(violations) > 0 {
				if op == "commit" {
					return fmt.Errorf("delivery contract changed: %s", strings.Join(violations, "; "))
				}
				c.Set("violations", violations)
				return nil
			}
			if op == "commit" {
				return commitResult(tx, row, &req.Result)
			}
		default:
			return fmt.Errorf("unknown run control operation")
		}
		return tx.Save(row).Error
	})
	if err != nil {
		controlFailure(c, err)
		return
	}
	if v, ok := c.Get("violations"); ok {
		c.JSON(200, gin.H{"violations": v})
		return
	}
	if op == "validate" || op == "commit" {
		c.JSON(200, gin.H{"result": req.Result})
		return
	}
	if op == "baseline" {
		c.JSON(200, gin.H{"output_baseline": req.OutputBaseline})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (s *Service) validateResult(ctx context.Context, tx *gorm.DB, row *RunRecord, result *ChatResult) ([]string, error) {
	violations := []string{}
	if result.RunID != row.ID {
		return nil, fmt.Errorf("result identity mismatch")
	}
	if strings.TrimSpace(result.Answer) == "" {
		violations = append(violations, "Answer must not be empty.")
	}
	// Validation keeps the candidate prose intact. Render once at durable commit,
	// after all finalization edits, so validate -> commit cannot duplicate tags.
	result.References = nil
	result.Usage = map[string]any{"model_requests": row.ModelRequests, "input_tokens": row.InputTokens, "output_tokens": row.OutputTokens, "unknown_usage_requests": row.UnknownUsageRequests, "reserved_tokens": row.ReservedTokens}
	var published []Artifact
	if err := tx.Where("run_id = ? AND tenant_id = ? AND storage_state = ?", row.ID, row.TenantID, artifactStorageStateReady).Order("created_at").Find(&published).Error; err != nil {
		return nil, err
	}
	result.Artifacts = nil
	for i := range published {
		result.Artifacts = append(result.Artifacts, *sidecarArtifactFromRow(&published[i]))
	}
	for i := range result.Artifacts {
		item := &result.Artifacts[i]
		var artifact Artifact
		if err := tx.Where("id = ? AND run_id = ? AND tenant_id = ? AND storage_state = ?", item.ArtifactID, row.ID, row.TenantID, artifactStorageStateReady).First(&artifact).Error; err != nil {
			return nil, fmt.Errorf("artifact is not durably published: %w", err)
		}
		if item.FileToken != artifact.FileToken || item.SHA256 != artifact.SHA256 || item.FileSize != artifact.FileSize {
			return nil, fmt.Errorf("artifact metadata mismatch")
		}
		*item = *sidecarArtifactFromRow(&artifact)
	}
	return violations, nil
}
func commitResult(tx *gorm.DB, row *RunRecord, result *ChatResult) error {
	if result.Status == "incomplete" {
		return storeResult(tx, row, result, "incomplete")
	}
	return storeResult(tx, row, result, "completed")
}

func storeResult(tx *gorm.DB, row *RunRecord, result *ChatResult, status string) error {
	answer, refsUsed, citations := sourcerefs.RenderStructuredCitations(result.Answer, result.Citations, row.References)
	if result.CitationStatus == "complete" {
		var submitted []sourcerefs.StructuredCitation
		if json.Unmarshal(result.Citations, &submitted) != nil || len(submitted) != len(citations) {
			// Fail closed on an internal mapping mismatch, never silently lose citations.
			result.CitationStatus, result.FailureCode, status = "failed", "citation_generation_failed", "incomplete"
			answer, refsUsed, citations = result.Answer, nil, nil
		}
	}
	result.Answer, result.References = answer, refsUsed
	result.Citations, _ = json.Marshal(citations)
	result.Status = status
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	refs, err := json.Marshal(result.References)
	if err != nil {
		return err
	}
	steps, err := runSteps(tx, row.ID)
	if err != nil {
		return err
	}
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	stats := sourcerefs.RetrievalStatsForAgentSteps(sourcerefs.RetrievalStatsFromReferences(row.References, sourcerefs.AgentStepsAttemptedRetrieval(steps)), steps)
	stats.CitationStatus = result.CitationStatus
	update := tx.Model(&types.Message{}).Where("id = ? AND session_id = ? AND role = 'assistant'", row.MessageID, row.SessionID).Updates(map[string]any{
		"error_code": "", "content": result.Answer, "knowledge_references": string(refs), "agent_steps": string(stepsJSON), "is_completed": true, "agent_duration_ms": time.Since(row.CreatedAt).Milliseconds(), "updated_at": time.Now(),
		"retrieval_stats": stats, "agent_tool_count": sourcerefs.AgentToolCallCount(steps),
	})
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return fmt.Errorf("assistant message unavailable at commit")
	}
	row.Status, row.Result = status, body
	if err = outbox(tx, row, StreamEvent{Type: "result", Data: body, Done: true}); err != nil {
		return err
	}
	return tx.Save(row).Error
}

// ModelLease governs a direct runtime->provider call. The socket's lifetime is
// exactly the provider request lifetime, including streaming and cancellation.
func (h *Handler) ModelLease(c *gin.Context) {
	if !validInternalAPIKey(c) {
		c.AbortWithStatus(401)
		return
	}
	if h.service.admission == nil {
		c.AbortWithStatus(503)
		return
	}
	ws, err := (&websocket.Upgrader{}).Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(65536)
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	var req controlRequest
	if err = ws.ReadJSON(&req); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	row, err := h.service.ownedRun(ctx, req.RunID, req.OwnerEpoch)
	if err != nil {
		_ = ws.WriteJSON(gin.H{"code": "ownership_lost", "error": "run fenced"})
		return
	}
	var scope RunScope
	if err = json.Unmarshal(row.Scope, &scope); err != nil {
		return
	}
	runCtx, err := h.service.executionContext(ctx, row)
	if err != nil {
		return
	}
	modelCtx := context.WithValue(runCtx, types.TenantIDContextKey, scope.ModelTenantID)
	modelID, kind := scope.ModelID, modeladmission.KindChat
	if req.ModelRole == "vision" {
		if scope.VLMModelID == "" {
			return
		}
		modelID, kind = scope.VLMModelID, modeladmission.KindVLM
	} else if req.ModelRole != "" {
		return
	}
	model, err := h.service.modelService.GetModelByID(modelCtx, modelID)
	if err != nil {
		_ = ws.WriteJSON(gin.H{"error": "model unavailable"})
		return
	}
	// Connection loss also cancels a request waiting for admission, before it
	// starts any provider work or consumes a model request from the run budget.
	outcome := make(chan struct {
		Status int    `json:"status"`
		Error  string `json:"error"`
		Usage  struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}, 1)
	go func() {
		var v struct {
			Status int    `json:"status"`
			Error  string `json:"error"`
			Usage  struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		_ = ws.SetReadDeadline(time.Time{})
		if ws.ReadJSON(&v) != nil {
			cancel()
			return
		}
		outcome <- v
	}()
	lease, err := h.service.admission.Acquire(modelCtx, modeladmission.SpecForModel(kind, model, ""))
	if err != nil {
		_ = ws.WriteJSON(gin.H{"error": "model admission unavailable"})
		return
	}
	defer lease.Release()
	call, err := h.service.reserveModelRequest(ctx, row.ID, req.OwnerEpoch, req.ModelRole, req.EstimatedInputTokens, req.MaxOutputTokens, req.Finalization)
	if err != nil {
		code, message := "control_unavailable", "Run budget service unavailable"
		switch {
		case errors.Is(err, errFinalizationRequired):
			code, message = "finalization_required", errFinalizationRequired.Error()
		case errors.Is(err, errModelBudget):
			code, message = "task_limit", errModelBudget.Error()
		case errors.Is(err, errRunFenced):
			code, message = "ownership_lost", "Run ownership lost"
		}
		_ = ws.WriteJSON(gin.H{"code": code, "error": message})
		return
	}
	accounted := false
	defer func() {
		if !accounted {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = h.service.finishModelRequest(cleanup, call, 0, 0)
		}
	}()
	if err = ws.WriteJSON(gin.H{"status": "acquired"}); err != nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case v := <-outcome:
			var failure error
			if v.Error != "" || v.Status >= 400 {
				failure = fmt.Errorf("provider status %d: %s", v.Status, v.Error)
			}
			accountingCtx, finishAccounting := context.WithTimeout(context.Background(), 5*time.Second)
			accounting := h.service.finishModelRequest(accountingCtx, call, v.Usage.InputTokens, v.Usage.OutputTokens)
			finishAccounting()
			if accounting != nil {
				return
			}
			accounted = true
			lease.Finish(failure)
			lease.Release()
			_ = ws.WriteJSON(gin.H{"status": "released"})
			return
		case <-ctx.Done():
			return
		case <-lease.Context().Done():
			return
		case <-ticker.C:
			if _, err = h.service.ownedRun(ctx, row.ID, req.OwnerEpoch); err != nil {
				return
			}
			if err = ws.WriteJSON(gin.H{"status": "alive"}); err != nil {
				return
			}
		}
	}
}

func (s *Service) followRun(ctx context.Context, initial *RunRecord, bus *event.EventBus) error {
	// The API observes a durable job. Only the explicit stop hook cancels it;
	// an API shutdown or lost observer cannot revoke the user's running job.
	return s.replayRun(ctx, initial, bus)
}

func (s *Service) replayRun(ctx context.Context, initial *RunRecord, bus *event.EventBus) error {
	cursor, revision := int64(0), int64(0)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		var events []RunOutbox
		if err := s.db.WithContext(ctx).Where("run_id = ? AND seq > ?", initial.ID, cursor).Order("seq").Limit(256).Find(&events).Error; err != nil {
			return err
		}
		for _, stored := range events {
			var item StreamEvent
			if err := json.Unmarshal(stored.Body, &item); err != nil {
				return err
			}
			cursor = stored.Seq
			switch item.Type {
			case "thought_delta", "commentary", "process_status":
				bus.Emit(ctx, event.Event{ID: processEventID(initial.ID, item), Type: event.EventAgentThought, SessionID: initial.SessionID, Data: event.AgentThoughtData{Content: item.Content, Iteration: int(item.Revision), Done: item.Done, Kind: item.Type, Sequence: stored.Seq}})
			case "answer_delta":
				// Candidate text is private until structured citation filtering at commit.
				continue
			case "bus":
				e, err := decodeBusEvent(item.Data)
				if err != nil {
					return err
				}
				bus.Emit(ctx, e)
			case "result":
				var result ChatResult
				if err := json.Unmarshal(item.Data, &result); err != nil {
					return err
				}
				steps, err := runSteps(s.db.WithContext(ctx), initial.ID)
				if err != nil {
					return err
				}
				bus.Emit(ctx, event.Event{ID: initial.MessageID, Type: event.EventAgentFinalAnswer, SessionID: initial.SessionID, Data: event.AgentFinalAnswerData{Content: result.Answer, Replace: true, Revision: revision + 1, Done: true}})
				bus.Emit(ctx, event.Event{Type: event.EventAgentComplete, SessionID: initial.SessionID, Data: event.AgentCompleteData{SessionID: initial.SessionID, MessageID: initial.MessageID, FinalAnswer: result.Answer, KnowledgeRefs: result.References, KnowledgeRefsAuthoritative: true, AgentSteps: steps, TotalSteps: len(steps), TotalDurationMs: time.Since(initial.CreatedAt).Milliseconds(), Extra: map[string]interface{}{"status": result.Status, "failure_code": result.FailureCode, "citation_status": result.CitationStatus, "artifacts": result.Artifacts, "artifact_notice": result.ArtifactNotice}}})
				return nil
			}
		}
		var row RunRecord
		if err := s.db.WithContext(ctx).Select("status", "error", "error_code", "result").First(&row, "id = ?", initial.ID).Error; err != nil {
			return err
		}
		if row.Status == "failed" || row.Status == "cancelled" || row.Status == "incomplete" {
			// A terminal result and status are committed atomically. Drain its
			// outbox even if this reader's previous page preceded that commit.
			if len(row.Result) > 0 {
				continue
			}
			return errors.New(usererrors.Classify(row.ErrorCode, row.Error).Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
