package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ModelRequest struct {
	ID             string `gorm:"primaryKey;type:varchar(36)"`
	RunID          string `gorm:"not null;index"`
	OwnerEpoch     int64
	Role           string
	Status         string
	InputTokens    int64
	OutputTokens   int64
	ReservedInput  int64
	ReservedOutput int64
	InputUnits     int64
	UsageUnknown   bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

var errModelBudget = errors.New("run model request budget exhausted")
var errFinalizationRequired = errors.New("final answer budget is reserved")

type BudgetState struct {
	RunID             string              `json:"run_id"`
	CurrentRunSources []map[string]string `json:"current_run_sources"`
	MaxTokens         int64               `json:"max_tokens"`
	RemainingTokens   int64               `json:"remaining_tokens"`
	MaxRequests       int64               `json:"max_requests"`
	RemainingRequests int64               `json:"remaining_requests"`
	FinalReserve      int64               `json:"final_reserve"`
	InputFactor       float64             `json:"input_factor"`
	RemainingSeconds  int64               `json:"remaining_seconds"`
}

// One authoritative ledger for primary and auxiliary calls, including recovery.
func modelBudget(tx *gorm.DB, row *RunRecord, role string) (BudgetState, error) {
	var p ChatPayload
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		return BudgetState{}, err
	}
	limit := budgetSetting("AGENT_RUNTIME_MAX_TOTAL_TOKENS", 3000000)
	requests := budgetSetting("AGENT_RUNTIME_MAX_MODEL_REQUESTS", 100)
	if p.RuntimeConfig.AgentType == "knowledge-qa" {
		limit = budgetSetting("AGENT_RUNTIME_KNOWLEDGE_QA_MAX_TOTAL_TOKENS", 1000000)
	} else if p.EnableArtifacts {
		limit = budgetSetting("AGENT_RUNTIME_ARTIFACT_MAX_TOTAL_TOKENS", 6000000)
		requests = budgetSetting("AGENT_RUNTIME_ARTIFACT_MAX_MODEL_REQUESTS", 200)
	}
	b := BudgetState{RunID: row.ID, CurrentRunSources: sourcerefs.StructuredCatalog(row.References), MaxTokens: limit, MaxRequests: requests, InputFactor: 1.25, FinalReserve: 32768}
	b.RemainingTokens = max(0, limit-row.InputTokens-row.OutputTokens-row.ReservedTokens)
	b.RemainingRequests = max(0, b.MaxRequests-int64(row.ModelRequests))
	b.RemainingSeconds = max(0, int64(time.Until(row.Deadline).Seconds()))
	var last ModelRequest
	err := tx.Where("run_id = ? AND role = ?", row.ID, "").Order("created_at DESC").Limit(1).Find(&last).Error
	if err != nil {
		return b, err
	}
	if last.ID != "" {
		b.FinalReserve = max(8192, last.ReservedInput+last.ReservedOutput)
	}
	var recent []ModelRequest
	if err := tx.Where("run_id = ? AND role = ? AND status = ? AND input_units > 0 AND usage_unknown = false", row.ID, role, "completed").Order("created_at DESC").Limit(8).Find(&recent).Error; err != nil {
		return b, err
	}
	if len(recent) > 0 {
		b.InputFactor = 0.5
		for _, call := range recent {
			b.InputFactor = math.Max(b.InputFactor, 1.15*float64(call.InputTokens)/float64(call.InputUnits))
		}
	}
	return b, nil
}

func (ModelRequest) TableName() string { return "custom_agent_model_requests" }

func sameJSON(a, b json.RawMessage) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

// A lost provider connection has unknown usage. Reconcile reservations when a
// new owner takes over; a late release from the former owner is then a no-op.
func reconcileModelRequests(tx *gorm.DB, row *RunRecord) error {
	var calls []ModelRequest
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("run_id = ? AND status = ? AND owner_epoch < ?", row.ID, "running", row.OwnerEpoch).Find(&calls).Error; err != nil {
		return err
	}
	for i := range calls {
		call := &calls[i]
		call.Status, call.UsageUnknown = "completed", true
		call.InputTokens, call.OutputTokens = call.ReservedInput, call.ReservedOutput
		row.ReservedTokens -= call.ReservedInput + call.ReservedOutput
		row.InputTokens += call.InputTokens
		row.OutputTokens += call.OutputTokens
		row.UnknownUsageRequests++
		if err := tx.Save(call).Error; err != nil {
			return err
		}
	}
	return nil
}

func budgetSetting(name string, fallback int64) int64 {
	if n, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil && n > 0 {
		return n
	}
	return fallback
}

func (s *Service) reserveModelRequest(ctx context.Context, runID string, epoch int64, role string, input, output int64, final ...bool) (*ModelRequest, error) {
	call := &ModelRequest{ID: uuid.NewString(), RunID: runID, OwnerEpoch: epoch, Role: role, Status: "running", ReservedInput: max(input, 1), ReservedOutput: max(output, 1)}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := lockRun(tx, runID)
		if err != nil {
			return err
		}
		if err = owned(row, epoch); err != nil {
			return err
		}
		b, err := modelBudget(tx, row, role)
		if err != nil {
			return err
		}
		call.InputUnits = max(input, 1)
		call.ReservedInput = max(1, int64(math.Ceil(float64(call.InputUnits)*b.InputFactor)))
		reserved := call.ReservedInput + call.ReservedOutput
		finalizing := len(final) > 0 && final[0] && role == ""
		if b.RemainingRequests < 1 || (finalizing && reserved > b.RemainingTokens) {
			return errModelBudget
		}
		reserve := b.FinalReserve
		if role == "" {
			reserve = max(reserve, reserved)
		}
		if !finalizing && (b.RemainingRequests <= 1 || b.RemainingTokens-reserved < reserve) {
			return errFinalizationRequired
		}
		row.ModelRequests++
		row.ReservedTokens += reserved
		if err = tx.Create(call).Error; err != nil {
			return err
		}
		return tx.Save(row).Error
	})
	return call, err
}

func (s *Service) finishModelRequest(ctx context.Context, call *ModelRequest, input, output int64) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := lockRun(tx, call.RunID)
		if err != nil {
			return err
		}
		var current ModelRequest
		if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", call.ID).Error; err != nil {
			return err
		}
		if current.Status != "running" {
			return nil
		}
		current.Status = "completed"
		current.UsageUnknown = input+output <= 0
		if current.UsageUnknown {
			input, output = current.ReservedInput, current.ReservedOutput
			row.UnknownUsageRequests++
		}
		current.InputTokens, current.OutputTokens = max(input, 0), max(output, 0)
		row.ReservedTokens -= current.ReservedInput + current.ReservedOutput
		row.InputTokens += current.InputTokens
		row.OutputTokens += current.OutputTokens
		if err = tx.Save(&current).Error; err != nil {
			return err
		}
		return tx.Save(row).Error
	})
}
