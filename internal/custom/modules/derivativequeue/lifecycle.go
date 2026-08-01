package derivativequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	MaxProviderAttempts    = 4
	MaxMaterializeAttempts = 8
	MaxFinalizeAttempts    = 20
)

func (r *Repository) Get(ctx context.Context, id string) (*WorkItem, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("derivative queue database is unavailable")
	}
	var row WorkItem
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *Repository) GetResult(ctx context.Context, workItemID string) (*Result, error) {
	var row Result
	err := r.db.WithContext(ctx).Where("work_item_id = ?", workItemID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &row, err
}

func (r *Repository) GetProviderCall(
	ctx context.Context,
	workItemID, requestHash string,
) (*ProviderCall, error) {
	var row ProviderCall
	err := r.db.WithContext(ctx).
		Where(
			"work_item_id = ? AND request_hash = ? AND disposition IN ?",
			workItemID, requestHash,
			[]string{ProviderCallCheckpointed, ProviderCallAccepted},
		).
		Order("attempt DESC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &row, err
}

func (r *Repository) FinalizerSiblings(
	ctx context.Context,
	item WorkItem,
) ([]WorkItem, bool, error) {
	var siblings []WorkItem
	if err := r.db.WithContext(ctx).
		Where(
			"tenant_id = ? AND knowledge_id = ? AND processing_generation = ? AND work_kind <> ?",
			item.TenantID, item.KnowledgeID, item.ProcessingGeneration, WorkFinalizer,
		).
		Order("item_id").
		Find(&siblings).Error; err != nil {
		return nil, false, err
	}
	for _, sibling := range siblings {
		switch sibling.State {
		case StateCompleted, StateFailed, StateProviderUnknown, StateCancelled:
		default:
			return siblings, false, nil
		}
	}
	return siblings, true, nil
}

func (r *Repository) SaveProviderCall(
	ctx context.Context,
	workItemID, leaseToken, requestHash, modelID, providerRequestID string,
	response []byte,
) (*ProviderCall, error) {
	if !json.Valid(response) {
		return nil, errors.New("provider response checkpoint must be valid JSON")
	}
	if len(response) > MaxPayloadSize {
		return nil, errors.New("provider response checkpoint exceeds 256 KiB object-storage threshold")
	}
	now := time.Now().UTC()
	sum := sha256.Sum256(response)
	var row ProviderCall
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item WorkItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND lease_token = ?", workItemID, leaseToken).
			First(&item).Error; err != nil {
			return ErrLeaseLost
		}
		if item.State != StateProviderRunning {
			return ErrInvalidState
		}
		if err := verifyGeneration(tx, item); err != nil {
			return err
		}
		var replayable ProviderCall
		replayErr := tx.Where(
			"work_item_id = ? AND request_hash = ? AND disposition IN ?",
			workItemID, requestHash,
			[]string{ProviderCallCheckpointed, ProviderCallAccepted},
		).Order("attempt DESC").First(&replayable).Error
		if replayErr == nil {
			if replayable.ContentChecksum != hex.EncodeToString(sum[:]) {
				return errors.New("provider checkpoint changed without contract rejection")
			}
			row = replayable
			return nil
		}
		if !errors.Is(replayErr, gorm.ErrRecordNotFound) {
			return replayErr
		}
		var maximum struct{ Attempt int }
		if err := tx.Model(&ProviderCall{}).
			Select("COALESCE(MAX(attempt), 0) AS attempt").
			Where("work_item_id = ? AND request_hash = ?", workItemID, requestHash).
			Scan(&maximum).Error; err != nil {
			return err
		}
		attempt := maximum.Attempt + 1
		row = ProviderCall{
			ID: uuid.NewSHA1(
				uuid.NameSpaceOID,
				[]byte(fmt.Sprintf("%s\x00%s\x00%d", workItemID, requestHash, attempt)),
			).String(),
			WorkItemID:  workItemID,
			RequestHash: requestHash,
			Attempt:     attempt,
			ProviderRequestKey: fmt.Sprintf(
				"derivative-%s:%s:%d",
				workItemID, requestHash[:min(16, len(requestHash))], attempt,
			),
			ProviderRequestID:    truncate(providerRequestID, 160),
			ProcessingGeneration: item.ProcessingGeneration,
			ModelID:              modelID,
			Response:             append(types.JSON(nil), response...),
			ResponseSize:         int64(len(response)),
			ContentChecksum:      hex.EncodeToString(sum[:]),
			Disposition:          ProviderCallCheckpointed,
			CreatedAt:            now,
		}
		return tx.Create(&row).Error
	})
	return &row, err
}

// RejectProviderCall marks the latest replayable response for one request as
// a deterministic contract failure. It remains immutable audit evidence but
// is no longer replayed; the next durable provider attempt may create attempt
// N+1 for the same request hash.
func (r *Repository) RejectProviderCall(
	ctx context.Context,
	workItemID, leaseToken, requestHash, validationError string,
) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item WorkItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND lease_token = ?", workItemID, leaseToken).
			First(&item).Error; err != nil {
			return ErrLeaseLost
		}
		if item.State != StateProviderRunning {
			return ErrInvalidState
		}
		if err := verifyGeneration(tx, item); err != nil {
			return err
		}
		var call ProviderCall
		if err := tx.Where(
			"work_item_id = ? AND request_hash = ? AND disposition IN ?",
			workItemID, requestHash,
			[]string{ProviderCallCheckpointed, ProviderCallAccepted},
		).Order("attempt DESC").First(&call).Error; err != nil {
			return err
		}
		result := tx.Model(&ProviderCall{}).
			Where("id = ? AND disposition IN ?", call.ID,
				[]string{ProviderCallCheckpointed, ProviderCallAccepted}).
			Updates(map[string]any{
				"disposition":      ProviderCallInvalidContract,
				"validation_error": truncate(validationError, 4096),
				"validated_at":     now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrInvalidState
		}
		return nil
	})
}

func (r *Repository) hasProviderCalls(ctx context.Context, workItemID string) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&ProviderCall{}).
		Where("work_item_id = ? AND disposition IN ?", workItemID,
			[]string{ProviderCallCheckpointed, ProviderCallAccepted}).
		Limit(1).Count(&count).Error
	return count > 0, err
}

// SealProviderExecution creates the immutable aggregate Result from the
// already-checkpointed outbound calls. No provider request occurs here.
func (r *Repository) SealProviderExecution(
	ctx context.Context,
	workItemID, leaseToken string,
) (*Result, error) {
	var calls []ProviderCall
	if err := r.db.WithContext(ctx).
		Where("work_item_id = ?", workItemID).
		Order("request_hash").
		Find(&calls).Error; err != nil {
		return nil, err
	}
	type callReceipt struct {
		ID                string `json:"id"`
		RequestHash       string `json:"request_hash"`
		Attempt           int    `json:"attempt"`
		Disposition       string `json:"disposition"`
		ProviderRequestID string `json:"provider_request_id,omitempty"`
		ResponseSize      int64  `json:"response_size"`
		ContentChecksum   string `json:"content_checksum"`
	}
	receipts := make([]callReceipt, 0, len(calls))
	for _, call := range calls {
		receipts = append(receipts, callReceipt{
			ID: call.ID, RequestHash: call.RequestHash, Attempt: call.Attempt,
			Disposition: call.Disposition, ProviderRequestID: call.ProviderRequestID,
			ResponseSize: call.ResponseSize, ContentChecksum: call.ContentChecksum,
		})
	}
	content, err := json.Marshal(map[string]any{"provider_calls": receipts})
	if err != nil {
		return nil, err
	}
	// A no-model/cache-hit task may still be leased. Move it to the provider
	// boundary without charging a provider attempt.
	result := r.db.WithContext(ctx).Model(&WorkItem{}).
		Where("id = ? AND lease_token = ? AND state = ?", workItemID, leaseToken, StateLeased).
		Updates(map[string]any{
			"state":                StateProviderRunning,
			"provider_request_key": "derivative-" + workItemID,
			"version":              gorm.Expr("version + 1"), "updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return nil, result.Error
	}
	return r.SaveProviderResult(ctx, workItemID, leaseToken, ProviderResult{
		Content: string(content),
		Metadata: types.JSON(fmt.Sprintf(
			`{"checkpointed_calls":%d}`, len(calls),
		)),
	})
}

func (r *Repository) CompleteMaterialization(
	ctx context.Context,
	workItemID, leaseToken string,
) error {
	return r.CompleteMaterializationOutcome(ctx, workItemID, leaseToken, "", "")
}

// CompleteMaterializationOutcome commits both the durable execution state
// and its business outcome. A generation fence is terminal cancellation, not
// a retryable materialization failure; persist that cancellation while the
// row is locked so the finalizer can never wait forever on a stale item.
func (r *Repository) CompleteMaterializationOutcome(
	ctx context.Context,
	workItemID, leaseToken, outcomeStatus, outcomeDetail string,
) error {
	outcomeStatus = strings.TrimSpace(outcomeStatus)
	if outcomeStatus != "" && outcomeStatus != "degraded" {
		return fmt.Errorf("complete derivative materialization: invalid outcome %q", outcomeStatus)
	}
	outcomeDetail = truncate(strings.TrimSpace(outcomeDetail), 2000)
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row WorkItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND lease_token = ?", workItemID, leaseToken).
			First(&row).Error; err != nil {
			return ErrLeaseLost
		}
		if row.State != StateProviderSucceeded {
			return ErrInvalidState
		}
		if err := verifyGeneration(tx, row); err != nil {
			if errors.Is(err, ErrGenerationFence) {
				return cancelStaleRow(tx, row, now)
			}
			return err
		}
		updates := map[string]any{
			"state": StateCompleted, "completed_at": now,
			"materialize_attempts": gorm.Expr("materialize_attempts + 1"),
			"finalize_attempts":    gorm.Expr("finalize_attempts + 1"),
			"owner_instance_id":    "", "lease_token": "", "lease_until": nil,
			"last_error_class": "", "last_error_code": "", "last_error_message": "",
			"outcome_status": outcomeStatus, "outcome_detail": outcomeDetail,
			"version": gorm.Expr("version + 1"), "updated_at": now,
		}
		result := tx.Model(&WorkItem{}).
			Where("id = ? AND version = ? AND lease_token = ?", row.ID, row.Version, leaseToken).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseLost
		}
		return tx.Model(&Result{}).Where("work_item_id = ?", row.ID).
			Update("materialized_at", now).Error
	})
}

func (r *Repository) CompleteFinalizer(
	ctx context.Context,
	workItemID, leaseToken string,
) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&WorkItem{}).
		Where(
			"id = ? AND lease_token = ? AND state = ? AND work_kind = ?",
			workItemID, leaseToken, StateLeased, WorkFinalizer,
		).
		Updates(map[string]any{
			"state": StateCompleted, "completed_at": now,
			"finalize_attempts": gorm.Expr("finalize_attempts + 1"),
			"owner_instance_id": "", "lease_token": "", "lease_until": nil,
			"last_error_class": "", "last_error_code": "", "last_error_message": "",
			"version": gorm.Expr("version + 1"), "updated_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (r *Repository) RetryAfterFailure(
	ctx context.Context,
	workItemID, leaseToken, errorClass, errorCode, message string,
	retryAfter time.Duration,
	forceProviderRetry bool,
) (terminal bool, err error) {
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	now := time.Now().UTC()
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row WorkItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND lease_token = ?", workItemID, leaseToken).
			First(&row).Error; err != nil {
			return ErrLeaseLost
		}
		hasCalls := false
		var count int64
		if err := tx.Model(&ProviderCall{}).
			Where("work_item_id = ? AND disposition IN ?", row.ID,
				[]string{ProviderCallCheckpointed, ProviderCallAccepted}).
			Count(&count).Error; err != nil {
			return err
		}
		hasCalls = count > 0
		attempts := row.ProviderAttempts
		maxAttempts := MaxProviderAttempts
		nextState := StateRetryWait
		if hasCalls && !forceProviderRetry {
			attempts = row.MaterializeAttempts + 1
			maxAttempts = MaxMaterializeAttempts
			nextState = StateMaterializeWait
		}
		if attempts >= maxAttempts {
			terminal = true
			nextState = StateFailed
		}
		updates := map[string]any{
			"state": nextState, "next_attempt_at": now.Add(retryAfter),
			"owner_instance_id": "", "lease_token": "", "lease_until": nil,
			"last_error_class":   truncate(errorClass, 32),
			"last_error_code":    truncate(errorCode, 64),
			"last_error_message": truncate(message, 4096),
			"version":            gorm.Expr("version + 1"), "updated_at": now,
		}
		if hasCalls && !forceProviderRetry {
			updates["materialize_attempts"] = attempts
		}
		if terminal {
			updates["completed_at"] = now
		}
		result := tx.Model(&WorkItem{}).
			Where("id = ? AND version = ? AND lease_token = ?", row.ID, row.Version, leaseToken).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseLost
		}
		return nil
	})
	return terminal, err
}

func (r *Repository) MarkProviderUnknown(
	ctx context.Context,
	workItemID string,
	message string,
) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&WorkItem{}).
		Where("id = ? AND state = ?", workItemID, StateProviderRunning).
		Updates(map[string]any{
			"state": StateProviderUnknown, "completed_at": now,
			"owner_instance_id": "", "lease_token": "", "lease_until": nil,
			"last_error_class": "provider", "last_error_code": "provider_outcome_unknown",
			"last_error_message": truncate(message, 4096),
			"version":            gorm.Expr("version + 1"), "updated_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrInvalidState
	}
	return nil
}

func (r *Repository) Heartbeat(
	ctx context.Context,
	workItemID, leaseToken string,
	ttl time.Duration,
) error {
	if ttl <= 0 {
		ttl = 45 * time.Second
	}
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&WorkItem{}).
		Where("id = ? AND lease_token = ? AND state IN ?",
			workItemID, leaseToken,
			[]string{StateLeased, StateProviderRunning, StateProviderSucceeded, StateMaterializing, StateFinalizing},
		).
		Updates(map[string]any{"lease_until": now.Add(ttl), "last_heartbeat_at": now, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (r *Repository) DeleteExpired(
	ctx context.Context,
	resultBefore, workBefore time.Time,
	batch int,
) (resultsDeleted, workDeleted int64, err error) {
	if batch < 1 || batch > 5000 {
		batch = 5000
	}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SET LOCAL lock_timeout = '500ms'").Error; err != nil {
				return err
			}
			if err := tx.Exec("SET LOCAL statement_timeout = '10s'").Error; err != nil {
				return err
			}
		}
		var resultIDs []string
		if err := tx.Model(&Result{}).Where("expires_at < ?", resultBefore).
			Order("expires_at").Limit(batch).Pluck("id", &resultIDs).Error; err != nil {
			return err
		}
		if len(resultIDs) > 0 {
			result := tx.Delete(&Result{}, "id IN ?", resultIDs)
			if result.Error != nil {
				return result.Error
			}
			resultsDeleted = result.RowsAffected
		}
		var workIDs []string
		if err := tx.Model(&WorkItem{}).
			Where("state IN ? AND completed_at < ?", []string{
				StateCompleted, StateCancelled, StateFailed, StateProviderUnknown,
			}, workBefore).
			Order("completed_at").Limit(batch).Pluck("id", &workIDs).Error; err != nil {
			return err
		}
		if len(workIDs) > 0 {
			if err := tx.Delete(&ProviderCall{}, "work_item_id IN ?", workIDs).Error; err != nil {
				return err
			}
			if err := tx.Delete(&Result{}, "work_item_id IN ?", workIDs).Error; err != nil {
				return err
			}
			result := tx.Delete(&WorkItem{}, "id IN ?", workIDs)
			if result.Error != nil {
				return result.Error
			}
			workDeleted = result.RowsAffected
		}
		return nil
	})
	return resultsDeleted, workDeleted, err
}
