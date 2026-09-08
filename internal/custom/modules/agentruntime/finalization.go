package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"time"
)

func (s *Service) deliveryRun(ctx context.Context, id string, epoch int64) (*RunRecord, error) {
	var row RunRecord
	if err := s.db.WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &row, ownedDelivery(&row, epoch)
}

// Finalization is a durable code-only stage. Its lease can be reclaimed without
// model credentials, and its I/O window is independent of the model deadline.
type Finalization struct {
	Result    ChatResult `json:"result"`
	Error     string     `json:"error,omitempty"`
	ErrorCode string     `json:"error_code,omitempty"`
}

const deliveryWindow = 10 * time.Minute

func ownedDelivery(row *RunRecord, epoch int64) error {
	if epoch < 1 || row.OwnerEpoch != epoch || row.Status != "finalizing" || !row.LeaseUntil.After(time.Now()) || !row.Deadline.After(time.Now()) {
		return errRunFenced
	}
	return nil
}

func beginFinalization(tx *gorm.DB, row *RunRecord, value Finalization) error {
	if value.Result.RunID != "" && value.Result.RunID != row.ID {
		return fmt.Errorf("finalization identity mismatch")
	}
	value.Result.RunID = row.ID
	if value.Result.Status == "" {
		value.Result.Status = "completed"
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	row.Finalization = raw
	row.Status = "finalizing"
	row.Deadline = time.Now().Add(deliveryWindow)
	return tx.Save(row).Error
}
