package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Tencent/WeKnora/internal/custom/modules/usererrors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const runLease = 30 * time.Second

var errRunFenced = errors.New("run is not owned, has expired, or is terminal")

// RunRecord is the authority for execution. Payload excludes provider and
// callback credentials: they are resolved afresh on each worker claim.
type RunRecord struct {
	ID                   string `gorm:"primaryKey;type:varchar(80)"`
	TenantID             uint64 `gorm:"not null;index"`
	UserID               string `gorm:"not null"`
	AccountID            string
	Principal            types.Principal `gorm:"serializer:json;type:jsonb"`
	SessionID            string          `gorm:"not null;index"`
	MessageID            string          `gorm:"not null;uniqueIndex"`
	Status               string          `gorm:"not null;index"`
	Owner                string
	OwnerEpoch           int64                 `gorm:"not null;default:0"`
	LeaseUntil           time.Time             `gorm:"index"`
	Deadline             time.Time             `gorm:"not null"`
	Payload              json.RawMessage       `gorm:"type:jsonb;not null"`
	Scope                json.RawMessage       `gorm:"type:jsonb;not null"`
	Checkpoint           json.RawMessage       `gorm:"type:jsonb"`
	Finalization         json.RawMessage       `gorm:"type:jsonb"`
	OutputBaseline       json.RawMessage       `gorm:"type:jsonb"`
	Result               json.RawMessage       `gorm:"type:jsonb"`
	References           []*types.SearchResult `gorm:"serializer:json;type:jsonb"`
	EventSeq             int64                 `gorm:"not null;default:0"`
	ClientSeq            int64                 `gorm:"not null;default:0"`
	Revision             int64                 `gorm:"not null;default:0"`
	ModelRequests        int                   `gorm:"not null;default:0"`
	InputTokens          int64                 `gorm:"not null;default:0"`
	OutputTokens         int64                 `gorm:"not null;default:0"`
	ReservedTokens       int64                 `gorm:"not null;default:0"`
	UnknownUsageRequests int                   `gorm:"not null;default:0"`
	Error                string
	ErrorCode            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func (RunRecord) TableName() string { return "custom_agent_runs" }

type RunScope struct {
	TenantRole          types.TenantRole         `json:"tenant_role"`
	Config              *types.AgentConfig       `json:"config"`
	ModelID             string                   `json:"model_id"`
	ModelTenantID       uint64                   `json:"model_tenant_id"`
	VLMModelID          string                   `json:"vlm_model_id"`
	ASRModelID          string                   `json:"asr_model_id"`
	SearchTargets       types.SearchTargets      `json:"search_targets"`
	PinnedMCPServiceIDs []string                 `json:"pinned_mcp_service_ids"`
	PinnedSkillNames    []string                 `json:"pinned_skill_names"`
	RuntimeAttachments  types.MessageAttachments `json:"attachments"`
	LightweightSkills   []LightweightSkillSpec   `json:"lightweight_skills"`
	NonInteractiveOAuth bool                     `json:"non_interactive_oauth"`
}

func (scope *RunScope) config() *types.AgentConfig {
	c := scope.Config
	c.RuntimeModelID, c.AgentTenantID, c.VLMModelID, c.ASRModelID = scope.ModelID, scope.ModelTenantID, scope.VLMModelID, scope.ASRModelID
	c.SearchTargets, c.PinnedMCPServiceIDs, c.PinnedSkillNames = scope.SearchTargets, scope.PinnedMCPServiceIDs, scope.PinnedSkillNames
	c.RuntimeAttachments = scope.RuntimeAttachments
	c.RuntimeLightweightSkills = nil
	for _, skill := range scope.LightweightSkills {
		c.RuntimeLightweightSkills = append(c.RuntimeLightweightSkills, types.RuntimeLightweightSkill{
			Key: skill.Key, Name: skill.Name, Description: skill.Description, Instructions: skill.Instructions, SelectedByUser: skill.SelectedByUser,
		})
	}
	return c
}

type RunOutbox struct {
	RunID     string          `gorm:"primaryKey;type:varchar(80)"`
	Seq       int64           `gorm:"primaryKey;autoIncrement:false"`
	Body      json.RawMessage `gorm:"type:jsonb;not null"`
	CreatedAt time.Time
}

func (RunOutbox) TableName() string { return "custom_agent_run_events" }

type ToolReceipt struct {
	Local      bool
	RunID      string          `gorm:"primaryKey;type:varchar(80)"`
	CallID     string          `gorm:"primaryKey;type:varchar(255)"`
	OwnerEpoch int64           `gorm:"not null"`
	Name       string          `gorm:"not null"`
	Arguments  json.RawMessage `gorm:"type:jsonb;not null"`
	Status     string          `gorm:"not null"`
	ReadOnly   bool
	Response   json.RawMessage `gorm:"type:jsonb"`
	DurationMs int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (ToolReceipt) TableName() string { return "custom_agent_tool_calls" }

func (s *Service) migrateRuns(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&RunRecord{}, &ToolReceipt{}, &RunOutbox{}, &ModelRequest{})
}

func lockRun(tx *gorm.DB, id string) (*RunRecord, error) {
	var row RunRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", id).Error
	return &row, err
}
func owned(row *RunRecord, epoch int64) error {
	if epoch < 1 || row.OwnerEpoch != epoch || row.Status != "running" || !row.LeaseUntil.After(time.Now()) || !row.Deadline.After(time.Now()) {
		return errRunFenced
	}
	return nil
}
func (s *Service) ownedRun(ctx context.Context, id string, epoch int64) (*RunRecord, error) {
	var row RunRecord
	if err := s.db.WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &row, owned(&row, epoch)
}
func runContext(ctx context.Context, row *RunRecord) context.Context {
	ctx = context.WithValue(ctx, types.TenantIDContextKey, row.TenantID)
	ctx = context.WithValue(ctx, types.SessionTenantIDContextKey, row.TenantID)
	ctx = context.WithValue(ctx, types.UserIDContextKey, row.AccountID)
	return types.WithPrincipal(ctx, row.Principal)
}

// Reload account rights at each execution boundary. Persisting an identity is
// not permission to retain a role after an administrator has revoked it.
func (s *Service) executionContext(ctx context.Context, row *RunRecord) (context.Context, error) {
	ctx = runContext(ctx, row)
	var tenant types.Tenant
	if err := s.db.WithContext(ctx).First(&tenant, "id = ?", row.TenantID).Error; err != nil {
		return ctx, fmt.Errorf("run tenant unavailable: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, &tenant)
	var scope RunScope
	if err := json.Unmarshal(row.Scope, &scope); err != nil {
		return ctx, err
	}
	role := scope.TenantRole
	user := &types.User{ID: row.AccountID, TenantID: row.TenantID, IsActive: true}
	if id, account := types.AccountUserIDFromContext(ctx); account {
		if err := s.db.WithContext(ctx).First(user, "id = ? AND is_active = true", id).Error; err != nil {
			return ctx, fmt.Errorf("run account unavailable: %w", err)
		}
		if user.IsSystemAdmin {
			role = types.TenantRoleOwner
		} else {
			var member types.TenantMember
			err := s.db.WithContext(ctx).First(&member, "user_id = ? AND tenant_id = ? AND status = ?", id, row.TenantID, types.TenantMemberStatusActive).Error
			if err == nil {
				role = member.Role
			} else if errors.Is(err, gorm.ErrRecordNotFound) && user.CanAccessAllTenants && user.TenantID != row.TenantID {
				role = types.TenantRoleAdmin
			} else {
				return ctx, fmt.Errorf("run membership unavailable: %w", err)
			}
		}
	}
	ctx = context.WithValue(ctx, types.UserContextKey, user)
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, role)
	return context.WithValue(ctx, types.SystemAdminContextKey, user.IsSystemAdmin), nil
}
func outbox(tx *gorm.DB, row *RunRecord, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	row.EventSeq++
	return tx.Create(&RunOutbox{RunID: row.ID, Seq: row.EventSeq, Body: encoded}).Error
}

func (s *Service) claimRun(ctx context.Context, worker string) (*ChatPayload, error) {
	var row *RunRecord
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Expired deadlines are terminal even if no worker was available to claim.
		var expired []RunRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("status IN ? AND deadline <= NOW()", []string{"queued", "running"}).Limit(32).Find(&expired).Error; err != nil {
			return err
		}
		for i := range expired {
			var payload ChatPayload
			if json.Unmarshal(expired[i].Payload, &payload) == nil && payload.EnableArtifacts && expired[i].Status == "running" {
				if err := beginFinalization(tx, &expired[i], Finalization{Result: ChatResult{RunID: expired[i].ID}, Error: "run deadline exceeded", ErrorCode: "task_limit"}); err != nil {
					return err
				}
				expired[i].LeaseUntil = time.Now().Add(-time.Second)
				if err := tx.Save(&expired[i]).Error; err != nil {
					return err
				}
				continue
			}
			if err := terminateRun(tx, &expired[i], "failed", "run deadline exceeded"); err != nil {
				return err
			}
		}
		var found RunRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("(deadline > NOW() AND (status = 'queued' OR (status = 'running' AND lease_until < NOW()))) OR (status = 'finalizing' AND (lease_until < NOW() OR deadline <= NOW()))").Order("created_at").First(&found).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // Commit expired jobs even when there is no new work.
		}
		if err != nil {
			return err
		}
		if found.Status == "finalizing" {
			found.Deadline = time.Now().Add(deliveryWindow)
		} else {
			found.Status = "running"
		}
		found.Owner = worker
		found.OwnerEpoch++
		found.LeaseUntil = time.Now().Add(runLease)
		if err := reconcileModelRequests(tx, &found); err != nil {
			return err
		}
		if err := tx.Save(&found).Error; err != nil {
			return err
		}
		row = &found
		return nil
	})
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, gorm.ErrRecordNotFound
	}
	var p ChatPayload
	var scope RunScope
	if err = json.Unmarshal(row.Payload, &p); err != nil {
		return nil, err
	}
	p.OwnerEpoch, p.DeadlineUnix = row.OwnerEpoch, float64(row.Deadline.UnixMilli())/1000
	p.OutputBaseline = row.OutputBaseline
	p.ToolCallbackURL, p.ArtifactUploadURL, p.ToolCallbackAPIKey = toolCallbackURL(), artifactUploadURL(), s.apiKey
	if row.Status == "finalizing" {
		if err = json.Unmarshal(row.Finalization, &p.Finalization); err != nil {
			return nil, err
		}
		return &p, nil
	}
	if err = json.Unmarshal(row.Scope, &scope); err != nil {
		return nil, err
	}
	if scope.Config == nil {
		_ = s.failRun(ctx, row.ID, row.OwnerEpoch, "runtime scope is unavailable")
		return nil, fmt.Errorf("runtime scope is unavailable")
	}
	runCtx, err := s.executionContext(ctx, row)
	if err != nil {
		_ = s.failRun(ctx, row.ID, row.OwnerEpoch, "run access is no longer available")
		return nil, err
	}
	p.LLM, err = s.resolveLLMConfig(runCtx, scope.config())
	if err != nil {
		_ = s.failRun(ctx, row.ID, row.OwnerEpoch, "model configuration unavailable")
		return nil, err
	}
	if !p.LLM.SupportsVision && scope.VLMModelID != "" {
		modelCtx := context.WithValue(runCtx, types.TenantIDContextKey, scope.ModelTenantID)
		visionModel, visionErr := s.modelService.GetModelByID(modelCtx, scope.VLMModelID)
		if visionErr != nil || visionModel == nil || visionModel.Type != types.ModelTypeVLLM {
			_ = s.failRun(ctx, row.ID, row.OwnerEpoch, "configured vision model is unavailable")
			return nil, fmt.Errorf("configured vision model is unavailable")
		}
		p.VisionLLM, err = runtimeProviderConfig(visionModel)
		if err != nil {
			_ = s.failRun(ctx, row.ID, row.OwnerEpoch, "vision model configuration unavailable")
			return nil, err
		}
	}
	p.OwnerEpoch, p.DeadlineUnix = row.OwnerEpoch, float64(row.Deadline.UnixMilli())/1000
	if err = s.prepareRuntimeMedia(runCtx, &p); err != nil {
		_ = s.failRun(ctx, row.ID, row.OwnerEpoch, "image input is unavailable")
		return nil, err
	}
	p.ToolCallbackURL, p.ArtifactUploadURL, p.ToolCallbackAPIKey = toolCallbackURL(), artifactUploadURL(), s.apiKey
	if len(row.Checkpoint) > 0 {
		if err = json.Unmarshal(row.Checkpoint, &p.Checkpoint); err != nil {
			return nil, err
		}
		// The outbox may be ahead of the last SDK snapshot after a crash.
		p.Checkpoint["event_seq"], p.Checkpoint["revision"] = row.ClientSeq, row.Revision
	}
	return &p, nil
}
func (s *Service) failRun(ctx context.Context, id string, epoch int64, reason string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := lockRun(tx, id)
		if err != nil {
			return err
		}
		if row.OwnerEpoch != epoch || row.Status != "running" {
			return errRunFenced
		}
		return terminateRun(tx, row, "failed", reason)
	})
}
func (s *Service) cancelRun(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := lockRun(tx, id)
		if err != nil {
			return err
		}
		if row.Status != "running" && row.Status != "queued" && row.Status != "finalizing" {
			return nil
		}
		return terminateRun(tx, row, "cancelled", "cancelled by caller")
	})
}

func terminateRun(tx *gorm.DB, row *RunRecord, status, reason string, codes ...string) error {
	row.Status, row.Error = status, reason
	code := ""
	if len(codes) > 0 {
		code = codes[0]
	}
	if status == "cancelled" {
		code = "cancelled"
	}
	failure := usererrors.Classify(code, reason)
	row.ErrorCode = failure.Code
	if failure.Code == "task_limit" {
		row.Status = "incomplete"
	}
	// Only persisted, scoped artifacts qualify as partial results. Never turn
	// a tool draft, validation error or interrupted message into a full success.
	var payload ChatPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return err
	}
	if payload.EnableArtifacts && status != "cancelled" {
		result, err := partialArtifacts(tx, row, failure.Code)
		if err != nil {
			return err
		}
		if result != nil {
			return storeResult(tx, row, result, "incomplete")
		}
	}
	if err := tx.Model(&types.Message{}).Where("id = ? AND session_id = ? AND role = 'assistant'", row.MessageID, row.SessionID).Updates(map[string]any{
		"is_completed": true, "content": failure.Message, "error_code": failure.Code, "updated_at": time.Now(),
	}).Error; err != nil {
		return err
	}
	return tx.Save(row).Error
}
func (s *Service) saveCheckpoint(ctx context.Context, id string, epoch int64, state json.RawMessage) error {
	var check struct {
		RunID         string `json:"run_id"`
		SDKVersion    string `json:"sdk_version"`
		EventSeq      int64  `json:"event_seq"`
		CheckpointSeq int64  `json:"checkpoint_seq"`
	}
	if err := json.Unmarshal(state, &check); err != nil {
		return err
	}
	if check.RunID != id || check.SDKVersion == "" {
		return fmt.Errorf("invalid checkpoint identity")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := lockRun(tx, id)
		if err != nil {
			return err
		}
		if err = owned(row, epoch); err != nil {
			return err
		}
		if check.EventSeq > row.ClientSeq {
			return fmt.Errorf("checkpoint precedes durable event barrier")
		}
		if len(row.Checkpoint) > 0 {
			var prior struct {
				CheckpointSeq int64 `json:"checkpoint_seq"`
			}
			if err = json.Unmarshal(row.Checkpoint, &prior); err != nil {
				return err
			}
			if check.CheckpointSeq <= prior.CheckpointSeq {
				if sameJSON(state, row.Checkpoint) {
					return nil
				}
				return fmt.Errorf("checkpoint sequence must advance")
			}
		}
		row.Checkpoint = state
		return tx.Save(row).Error
	})
}
