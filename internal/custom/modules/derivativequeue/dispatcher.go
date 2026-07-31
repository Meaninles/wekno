package derivativequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	TypeWake         = "custom:derivative:wake"
	dispatchBatch    = 500
	dispatchCooldown = 5 * time.Second
)

func (r *Repository) hydrateRoute(ctx context.Context, item *PlanItem) error {
	if item == nil {
		return errors.New("derivative plan item is required")
	}
	if item.WorkKind == WorkFinalizer {
		return nil
	}
	if strings.TrimSpace(item.ModelID) != "" && item.ModelTenantID > 0 &&
		strings.TrimSpace(item.ResourcePoolID) != "" {
		return nil
	}
	modelID := strings.TrimSpace(item.ModelID)
	if modelID == "" {
		var config struct {
			DefaultModelID string
		}
		if err := r.db.WithContext(ctx).
			Table("custom_derivative_control_configs").
			Select("default_model_id").Where("id = 1").
			Take(&config).Error; err != nil {
			return fmt.Errorf("resolve derivative default model: %w", err)
		}
		modelID = strings.TrimSpace(config.DefaultModelID)
	}
	if modelID == "" {
		return errors.New("no derivative model is configured")
	}
	var assignment struct {
		ModelID       string
		ModelTenantID uint64
	}
	if err := r.db.WithContext(ctx).
		Table("custom_derivative_model_assignments").
		Where("model_id = ?", modelID).
		Take(&assignment).Error; err != nil {
		return fmt.Errorf("resolve published derivative model: %w", err)
	}
	var model types.Model
	if err := r.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND deleted_at IS NULL",
			assignment.ModelID, assignment.ModelTenantID).
		Take(&model).Error; err != nil {
		return fmt.Errorf("load published derivative model: %w", err)
	}
	item.ModelID = model.ID
	item.ModelTenantID = model.TenantID
	if r.admission == nil {
		return nil
	}
	policy, err := r.admission.ResolvePolicy(
		ctx,
		modeladmission.SpecForModel(modeladmission.KindDerivative, &model, ""),
	)
	if err != nil {
		return fmt.Errorf("resolve derivative admission route: %w", err)
	}
	item.ResourcePoolID = policy.PoolID
	item.QuotaPoolID = policy.QuotaPoolID
	item.GatewayPoolID = policy.GatewayPoolID
	item.PolicyVersion = policy.PolicyVersion
	return nil
}

// PublishPlan is the only derivative fan-out publication path. PostgreSQL is
// committed first; the Asynq message contains only the work-item identity and
// dispatch epoch.
func (r *Repository) PublishPlan(
	ctx context.Context,
	enqueuer interfaces.TaskEnqueuer,
	items []PlanItem,
) ([]WorkItem, error) {
	if enqueuer == nil && len(items) > 0 {
		return nil, errors.New("derivative wake enqueuer is unavailable")
	}
	resolved := make([]PlanItem, len(items))
	copy(resolved, items)
	for index := range resolved {
		if err := r.hydrateRoute(ctx, &resolved[index]); err != nil {
			return nil, err
		}
	}
	rows, err := r.UpsertPlan(ctx, resolved)
	if err != nil {
		return nil, err
	}
	for index := range rows {
		switch rows[index].State {
		case StateCompleted, StateCancelled, StateFailed, StateProviderUnknown:
			continue
		}
		if rows[index].NextAttemptAt.After(time.Now().UTC()) {
			continue
		}
		wake, markErr := r.MarkDispatched(ctx, rows[index].ID, rows[index].Version)
		if errors.Is(markErr, ErrInvalidState) {
			continue
		}
		if markErr != nil {
			return rows, markErr
		}
		if err := enqueueWake(enqueuer, wake); err != nil &&
			!errors.Is(err, asynq.ErrTaskIDConflict) {
			return rows, fmt.Errorf("enqueue derivative wake: %w", err)
		}
		rows[index].DispatchEpoch = wake.DispatchEpoch
		rows[index].DispatchTaskID = wakeTaskID(wake)
		rows[index].Version++
	}
	return rows, nil
}

func wakeTaskID(wake WakePayload) string {
	return fmt.Sprintf("derivative:%s:%d", wake.WorkItemID, wake.DispatchEpoch)
}

func enqueueWake(enqueuer interfaces.TaskEnqueuer, wake WakePayload) error {
	raw, err := json.Marshal(wake)
	if err != nil {
		return err
	}
	_, err = enqueuer.Enqueue(
		asynq.NewTask(TypeWake, raw),
		asynq.Queue(types.QueueDerivative),
		asynq.TaskID(wakeTaskID(wake)),
		asynq.MaxRetry(0),
		asynq.Timeout(30*time.Minute),
		asynq.Retention(24*time.Hour),
	)
	return err
}

// DispatchDue republishes due PostgreSQL rows. It is safe to run from more
// than one process: MarkDispatched uses a row lock/version CAS and Claim fences
// every older epoch.
func (r *Repository) DispatchDue(
	ctx context.Context,
	enqueuer interfaces.TaskEnqueuer,
	limit int,
) (int, error) {
	if limit < 1 || limit > dispatchBatch {
		limit = dispatchBatch
	}
	now := time.Now().UTC()
	var candidates []WorkItem
	fetchLimit := limit * 8
	if fetchLimit < 64 {
		fetchLimit = 64
	}
	if fetchLimit > 4000 {
		fetchLimit = 4000
	}
	if err := r.db.WithContext(ctx).
		Where("state IN ? AND next_attempt_at <= ? AND updated_at <= ?",
			[]string{StateQueued, StateRetryWait, StateMaterializeWait, StateFinalizeWait},
			now, now.Add(-dispatchCooldown)).
		Order("next_attempt_at, created_at").
		Limit(fetchLimit).Find(&candidates).Error; err != nil {
		return 0, err
	}
	candidates = r.fair.selectCandidates(candidates, limit, now)
	dispatched := 0
	for _, candidate := range candidates {
		wake, err := r.MarkDispatched(ctx, candidate.ID, candidate.Version)
		if errors.Is(err, ErrInvalidState) {
			continue
		}
		if err != nil {
			return dispatched, err
		}
		if err := enqueueWake(enqueuer, wake); err != nil &&
			!errors.Is(err, asynq.ErrTaskIDConflict) {
			return dispatched, err
		}
		dispatched++
	}
	return dispatched, nil
}

// RecoverExpiredLeases returns items that reached an operator-visible
// provider_unknown terminal state. Other expired leases are safely returned
// to a due queue for replay/materialization.
func (r *Repository) RecoverExpiredLeases(
	ctx context.Context,
	limit int,
) ([]WorkItem, error) {
	if limit < 1 || limit > dispatchBatch {
		limit = dispatchBatch
	}
	now := time.Now().UTC()
	var candidates []WorkItem
	if err := r.db.WithContext(ctx).
		Where("lease_until IS NOT NULL AND lease_until < ? AND state IN ?", now, []string{
			StateLeased, StateProviderRunning, StateProviderSucceeded,
			StateMaterializing, StateFinalizing,
		}).
		Order("lease_until").Limit(limit).Find(&candidates).Error; err != nil {
		return nil, err
	}
	unknown := make([]WorkItem, 0)
	for _, candidate := range candidates {
		err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var row WorkItem
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
				Where("id = ?", candidate.ID).First(&row).Error; err != nil {
				return err
			}
			if row.LeaseUntil == nil || row.LeaseUntil.After(now) {
				return nil
			}
			var callCount int64
			if err := tx.Model(&ProviderCall{}).Where("work_item_id = ?", row.ID).
				Count(&callCount).Error; err != nil {
				return err
			}
			nextState := StateQueued
			switch row.State {
			case StateProviderRunning:
				if callCount == 0 {
					nextState = StateProviderUnknown
				} else {
					nextState = StateMaterializeWait
				}
			case StateProviderSucceeded, StateMaterializing:
				nextState = StateMaterializeWait
			case StateFinalizing:
				nextState = StateFinalizeWait
			}
			updates := map[string]any{
				"state": nextState, "next_attempt_at": now,
				"owner_instance_id": "", "lease_token": "", "lease_until": nil,
				"version": gorm.Expr("version + 1"), "updated_at": now,
			}
			if nextState == StateProviderUnknown {
				updates["completed_at"] = now
				updates["last_error_class"] = "provider"
				updates["last_error_code"] = "provider_outcome_unknown"
				updates["last_error_message"] = "worker lease expired after provider start and before response checkpoint"
				row.State = nextState
				row.CompletedAt = &now
				unknown = append(unknown, row)
			}
			return tx.Model(&WorkItem{}).
				Where("id = ? AND version = ?", row.ID, row.Version).
				Updates(updates).Error
		})
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return unknown, err
		}
	}
	return unknown, nil
}

// RefreshMetrics takes one bounded-cardinality snapshot from the authoritative
// table. It runs only on the maintenance leader.
func (r *Repository) RefreshMetrics(ctx context.Context) error {
	now := time.Now().UTC()
	var rows []struct {
		State          string  `gorm:"column:state"`
		Kind           string  `gorm:"column:kind"`
		Pool           string  `gorm:"column:pool"`
		Count          int64   `gorm:"column:count"`
		OldestWaitSecs float64 `gorm:"column:oldest_wait_secs"`
	}
	if err := r.db.WithContext(ctx).Model(&WorkItem{}).
		Select(`state, work_kind AS kind, resource_pool_id AS pool, COUNT(*) AS count,
			CASE WHEN state IN ('queued','retry_wait','materialize_wait','finalize_wait')
			THEN EXTRACT(EPOCH FROM (? - MIN(created_at))) ELSE 0 END AS oldest_wait_secs`, now).
		Group("state, work_kind, resource_pool_id").
		Scan(&rows).Error; err != nil {
		return err
	}
	snapshot := make([]pipelineobs.DerivativeMetricRow, 0, len(rows))
	for _, row := range rows {
		snapshot = append(snapshot, pipelineobs.DerivativeMetricRow{
			State: row.State, Kind: row.Kind, Pool: row.Pool,
			Count: row.Count, OldestWaitSeconds: row.OldestWaitSecs,
		})
	}
	pipelineobs.SetDerivativeSnapshot(snapshot)
	return nil
}
