package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"time"

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
	UsageUnknown   bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
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

func (s *Service) reserveModelRequest(ctx context.Context, runID string, epoch int64, role string, input, output int64) (*ModelRequest, error) {
	call := &ModelRequest{ID: uuid.NewString(), RunID: runID, OwnerEpoch: epoch, Role: role, Status: "running", ReservedInput: max(input, 1), ReservedOutput: max(output, 1)}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := lockRun(tx, runID)
		if err != nil {
			return err
		}
		if err = owned(row, epoch); err != nil {
			return err
		}
		reserved := call.ReservedInput + call.ReservedOutput
		if int64(row.ModelRequests) >= budgetSetting("AGENT_RUNTIME_MAX_MODEL_REQUESTS", 100) || row.InputTokens+row.OutputTokens+row.ReservedTokens+reserved > budgetSetting("AGENT_RUNTIME_MAX_TOTAL_TOKENS", 1000000) {
			return fmt.Errorf("run model request budget exhausted")
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
