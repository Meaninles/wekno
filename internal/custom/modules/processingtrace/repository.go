package processingtrace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
)

const MaxPageSize = 500

type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// LogicalKey returns the stable storage identity for one business span. The
// display name is deliberately kept separate from the row identity so a
// retry/redelivery updates the same row instead of creating delivery history.
func LogicalKey(kind, name string) string {
	switch {
	case kind == "root":
		return "root"
	case kind == "stage":
		return "stage:" + name
	case name == "postprocess.summary":
		return "derivative:summary"
	case strings.HasPrefix(name, "postprocess.question.batch["):
		index := strings.TrimSuffix(strings.TrimPrefix(name, "postprocess.question.batch["), "]")
		return "derivative:question_batch:" + index
	case strings.HasPrefix(name, "postprocess.graph.chunk["):
		index := strings.TrimSuffix(strings.TrimPrefix(name, "postprocess.graph.chunk["), "]")
		return "derivative:graph_batch:" + index
	case strings.HasPrefix(name, "postprocess.wiki"):
		return "wiki:" + name
	default:
		return kind + ":" + name
	}
}

// DeterministicSpanID is stable for the lifetime of one logical span. It is
// also used by in-memory handles, which lets children name a parent before the
// parent has been persisted by the provider-start observer.
func DeterministicSpanID(knowledgeID string, attempt int, logicalKey string) string {
	return uuid.NewSHA1(
		uuid.NameSpaceOID,
		[]byte(fmt.Sprintf("%s\x00%d\x00%s", knowledgeID, attempt, logicalKey)),
	).String()
}

func (r *Repository) Migrate(ctx context.Context) error {
	if r == nil || r.db == nil {
		return errors.New("processing trace database is unavailable")
	}
	db := r.db.WithContext(ctx)
	if err := db.AutoMigrate(&Span{}); err != nil {
		return err
	}
	if !db.Migrator().HasTable("knowledges") {
		return nil
	}
	hadCoreStatus := db.Migrator().HasColumn(&knowledgeLifecycleSchema{}, "core_status")
	if err := db.AutoMigrate(&knowledgeLifecycleSchema{}); err != nil {
		return fmt.Errorf("migrate knowledge lifecycle columns: %w", err)
	}
	// Only perform the legacy-row backfill on the first installation. Repeated
	// migration runs must never derive a newer explicit lifecycle state from
	// parse_status and overwrite it.
	if !hadCoreStatus {
		if err := db.Exec(`
			UPDATE knowledges
			SET core_status = CASE
					WHEN parse_status IN ('completed', 'finalizing') THEN 'ready'
					WHEN parse_status = 'failed' THEN 'failed'
					WHEN parse_status = 'processing' THEN 'processing'
					ELSE 'pending'
				END,
				core_completed_at = CASE
					WHEN parse_status IN ('completed', 'finalizing')
						THEN COALESCE(processed_at, updated_at, created_at)
					ELSE NULL
				END,
				enrichment_completed_at = CASE
					WHEN enrichment_status IN ('completed', 'degraded', 'failed')
						THEN COALESCE(processed_at, updated_at, created_at)
					ELSE NULL
				END
		`).Error; err != nil {
			return fmt.Errorf("backfill knowledge lifecycle columns: %w", err)
		}
	}
	if err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_knowledges_core_retrieval
		ON knowledges (tenant_id, knowledge_base_id, core_status, id)
		WHERE deleted_at IS NULL AND enable_status = 'enabled'
	`).Error; err != nil {
		return fmt.Errorf("create knowledge core retrieval index: %w", err)
	}
	// Builds before the core/derivative lifecycle split left a document in
	// finalizing until optional work (and, historically, Wiki) terminated. The
	// post-process receipt proves every downstream intent was durably published,
	// so these generations can be promoted without dropping counters or outcome
	// state. Run on every migration; the predicate makes it idempotent.
	if db.Migrator().HasTable("knowledge_fanout_completions") {
		if err := db.Exec(`
			UPDATE knowledges
			SET parse_status = 'completed',
				processing_owner = '',
				processing_fanout = NULL,
				processed_at = COALESCE(processed_at, CURRENT_TIMESTAMP),
				updated_at = CURRENT_TIMESTAMP
			WHERE parse_status = 'finalizing'
			  AND EXISTS (
				SELECT 1
				FROM knowledge_fanout_completions AS completion
				WHERE completion.tenant_id = knowledges.tenant_id
				  AND completion.knowledge_id = knowledges.id
				  AND completion.knowledge_base_id = knowledges.knowledge_base_id
				  AND completion.processing_generation = knowledges.processing_generation
				  AND completion.item_id = 'orchestration:postprocess'
			  )
		`).Error; err != nil {
			return fmt.Errorf("promote receipt-backed legacy finalizing generations: %w", err)
		}
	}
	return nil
}

// RecordBusinessProgress upserts one stable logical span. Control-plane waits
// (admission, pacing, circuit, dispatcher fairness, duplicate delivery) have
// no API here by design and therefore cannot create span-write amplification.
func (r *Repository) RecordBusinessProgress(ctx context.Context, input Upsert) error {
	if strings.TrimSpace(input.KnowledgeID) == "" ||
		input.Attempt < 1 || strings.TrimSpace(input.LogicalKey) == "" {
		return errors.New("knowledge_id, positive attempt, and logical_key are required")
	}
	now := time.Now().UTC()
	started := input.StartedAt
	if started.IsZero() {
		started = now
	}
	spanID := DeterministicSpanID(input.KnowledgeID, input.Attempt, input.LogicalKey)
	updates := map[string]any{
		"parent_logical_key": input.ParentLogicalKey,
		"name":               input.Name, "kind": input.Kind, "status": input.Status,
		"last_error_code":           truncate(input.LastErrorCode, 64),
		"last_error_message":        truncate(input.LastErrorMessage, 4096),
		"last_error_detail":         truncate(input.LastErrorDetail, 8192),
		"last_business_progress_at": input.LastBusinessProgressAt,
		"finished_at":               input.FinishedAt, "updated_at": now,
	}
	// Begin/End/Fail callers intentionally send sparse payloads. Empty content
	// therefore means "preserve the previous value", not "erase it".
	if strings.TrimSpace(input.InputSummary) != "" {
		updates["input_summary"] = truncate(input.InputSummary, 4096)
	}
	if strings.TrimSpace(input.OutputSummary) != "" {
		updates["output_summary"] = truncate(input.OutputSummary, 4096)
	}
	if strings.TrimSpace(input.MetadataSummary) != "" {
		updates["metadata_summary"] = truncate(input.MetadataSummary, 4096)
	}
	if input.FinishedAt != nil {
		updates["duration_ms"] = input.FinishedAt.Sub(started).Milliseconds()
	} else if input.Status == "pending" || input.Status == "running" {
		updates["duration_ms"] = int64(0)
	}
	if input.IncrementRealAttempt {
		updates["real_attempt_count"] = gorm.Expr("custom_processing_spans_v2.real_attempt_count + 1")
	} else if input.DecrementRealAttempt {
		updates["real_attempt_count"] = gorm.Expr(
			"CASE WHEN custom_processing_spans_v2.real_attempt_count > 0 THEN custom_processing_spans_v2.real_attempt_count - 1 ELSE 0 END",
		)
	}
	initialAttempts := 0
	if input.IncrementRealAttempt {
		initialAttempts = 1
	}
	row := Span{
		KnowledgeID: input.KnowledgeID, Attempt: input.Attempt,
		LogicalKey: input.LogicalKey, SpanID: spanID,
		ParentLogicalKey: input.ParentLogicalKey, Name: input.Name,
		Kind: input.Kind, Status: input.Status, RealAttemptCount: initialAttempts,
		InputSummary:     truncate(input.InputSummary, 4096),
		OutputSummary:    truncate(input.OutputSummary, 4096),
		MetadataSummary:  truncate(input.MetadataSummary, 4096),
		LastErrorCode:    truncate(input.LastErrorCode, 64),
		LastErrorMessage: truncate(input.LastErrorMessage, 4096),
		LastErrorDetail:  truncate(input.LastErrorDetail, 8192),
		StartedAt:        started, LastBusinessProgressAt: input.LastBusinessProgressAt,
		FinishedAt: input.FinishedAt, CreatedAt: now, UpdatedAt: now,
	}
	if input.FinishedAt != nil {
		row.DurationMS = input.FinishedAt.Sub(started).Milliseconds()
	}
	if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "knowledge_id"}, {Name: "attempt"}, {Name: "logical_key"},
		},
		DoUpdates: clause.Assignments(updates),
	}).Create(&row).Error; err != nil {
		return err
	}
	// Exact insert-vs-update classification previously issued a COUNT before
	// every write, doubling trace query volume and racing concurrent upserts.
	// Row cardinality is refreshed separately; count this as an update event
	// and let the periodic gauge expose inserts without an extra hot-path read.
	pipelineobs.ProcessingSpanUpdated()
	return nil
}

func (r *Repository) GetBySpanID(ctx context.Context, knowledgeID string, attempt int, spanID string) (*Span, error) {
	var row Span
	err := r.db.WithContext(ctx).
		Where("knowledge_id = ? AND attempt = ? AND span_id = ?", knowledgeID, attempt, spanID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *Repository) GetByLogicalKey(ctx context.Context, knowledgeID string, attempt int, logicalKey string) (*Span, error) {
	var row Span
	err := r.db.WithContext(ctx).
		Where("knowledge_id = ? AND attempt = ? AND logical_key = ?", knowledgeID, attempt, logicalKey).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *Repository) List(
	ctx context.Context,
	knowledgeID string,
	attempt int,
	limit int,
	cursor *Cursor,
) (Page, error) {
	knowledgeID = strings.TrimSpace(knowledgeID)
	if knowledgeID == "" {
		return Page{}, errors.New("knowledge_id is required")
	}
	if attempt < 1 {
		return Page{}, errors.New("a positive attempt is required")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	query := r.db.WithContext(ctx).
		Where("knowledge_id = ? AND attempt = ?", knowledgeID, attempt)
	if cursor != nil && strings.TrimSpace(cursor.LogicalKey) != "" {
		query = query.Where("logical_key > ?", cursor.LogicalKey)
	}
	var rows []Span
	if err := query.Order("logical_key").Limit(limit + 1).Find(&rows).Error; err != nil {
		return Page{}, err
	}
	page := Page{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		page.NextCursor = &Cursor{LogicalKey: page.Items[len(page.Items)-1].LogicalKey}
	}
	return page, nil
}

func (r *Repository) LatestAttempt(ctx context.Context, knowledgeID string) (int, error) {
	var attempt int
	err := r.db.WithContext(ctx).Model(&Span{}).
		Where("knowledge_id = ?", strings.TrimSpace(knowledgeID)).
		Select("COALESCE(MAX(attempt), 0)").Scan(&attempt).Error
	return attempt, err
}

// AllocateAttemptRoot serializes per-document attempt allocation and creates
// the V2 root in the same transaction. No legacy trace row participates in
// allocation, so the old physical-delivery table can be removed safely.
func (r *Repository) AllocateAttemptRoot(
	ctx context.Context,
	input Upsert,
	errorCode, reason string,
) (Span, int64, error) {
	if r == nil || r.db == nil || strings.TrimSpace(input.KnowledgeID) == "" {
		return Span{}, 0, errors.New("processing trace repository and knowledge_id are required")
	}
	var root Span
	var superseded int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
			if err := tx.Exec(
				"SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
				"weknora:processing-trace-attempt:"+input.KnowledgeID,
			).Error; err != nil {
				return fmt.Errorf("lock processing trace attempt: %w", err)
			}
		}
		var maxAttempt int
		if err := tx.Model(&Span{}).
			Where("knowledge_id = ?", input.KnowledgeID).
			Select("COALESCE(MAX(attempt), 0)").Scan(&maxAttempt).Error; err != nil {
			return fmt.Errorf("read latest processing trace attempt: %w", err)
		}
		attempt := maxAttempt + 1
		now := time.Now().UTC()
		result := tx.Model(&Span{}).
			Where("knowledge_id = ? AND attempt < ? AND status IN ?",
				input.KnowledgeID, attempt, []string{"pending", "running"}).
			Updates(map[string]any{
				"status": "cancelled", "last_error_code": truncate(errorCode, 64),
				"last_error_message": truncate(reason, 4096),
				"finished_at":        now, "updated_at": now,
			})
		if result.Error != nil {
			return fmt.Errorf("supersede older processing traces: %w", result.Error)
		}
		superseded = result.RowsAffected
		started := input.StartedAt
		if started.IsZero() {
			started = now
		}
		root = Span{
			KnowledgeID: input.KnowledgeID, Attempt: attempt, LogicalKey: "root",
			SpanID:           DeterministicSpanID(input.KnowledgeID, attempt, "root"),
			ParentLogicalKey: "", Name: input.Name, Kind: input.Kind, Status: input.Status,
			RealAttemptCount: 1,
			InputSummary:     truncate(input.InputSummary, 4096),
			OutputSummary:    truncate(input.OutputSummary, 4096),
			MetadataSummary:  truncate(input.MetadataSummary, 4096),
			LastErrorCode:    truncate(input.LastErrorCode, 64),
			LastErrorMessage: truncate(input.LastErrorMessage, 4096),
			LastErrorDetail:  truncate(input.LastErrorDetail, 8192),
			StartedAt:        started, LastBusinessProgressAt: input.LastBusinessProgressAt,
			FinishedAt: input.FinishedAt, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&root).Error; err != nil {
			return fmt.Errorf("create processing trace root: %w", err)
		}
		return nil
	})
	return root, superseded, err
}

func (r *Repository) SupersedeOlderAttempts(
	ctx context.Context,
	knowledgeID string,
	attempt int,
	errorCode, reason string,
) (int64, error) {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&Span{}).
		Where("knowledge_id = ? AND attempt < ? AND status IN ?",
			knowledgeID, attempt, []string{"pending", "running"}).
		Updates(map[string]any{
			"status": "cancelled", "last_error_code": truncate(errorCode, 64),
			"last_error_message": truncate(reason, 4096),
			"finished_at":        now, "updated_at": now,
		})
	return result.RowsAffected, result.Error
}

func (r *Repository) CancelOpen(
	ctx context.Context,
	knowledgeID string,
	attempt int,
	logicalKeys []string,
	errorCode, reason string,
) (int64, error) {
	query := r.db.WithContext(ctx).Model(&Span{}).
		Where("knowledge_id = ? AND attempt = ? AND status IN ?",
			knowledgeID, attempt, []string{"pending", "running"})
	if len(logicalKeys) > 0 {
		query = query.Where("logical_key IN ?", logicalKeys)
	}
	now := time.Now().UTC()
	result := query.Updates(map[string]any{
		"status": "cancelled", "last_error_code": truncate(errorCode, 64),
		"last_error_message": truncate(reason, 4096),
		"finished_at":        now, "updated_at": now,
	})
	return result.RowsAffected, result.Error
}

func (r *Repository) CancelOpenByName(
	ctx context.Context,
	knowledgeID string,
	attempt int,
	name, errorCode, reason string,
) (int64, error) {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&Span{}).
		Where("knowledge_id = ? AND attempt = ? AND name = ? AND status IN ?",
			knowledgeID, attempt, name, []string{"pending", "running"}).
		Updates(map[string]any{
			"status": "cancelled", "last_error_code": truncate(errorCode, 64),
			"last_error_message": truncate(reason, 4096),
			"finished_at":        now, "updated_at": now,
		})
	return result.RowsAffected, result.Error
}

func (r *Repository) DeleteKnowledge(ctx context.Context, knowledgeID string) error {
	return r.db.WithContext(ctx).Delete(&Span{}, "knowledge_id = ?", strings.TrimSpace(knowledgeID)).Error
}

func (r *Repository) DeleteExpired(ctx context.Context, before time.Time, batch int) (int64, error) {
	if batch < 1 || batch > 5000 {
		batch = 5000
	}
	var keys []Span
	err := r.db.WithContext(ctx).
		Select("knowledge_id", "attempt", "logical_key").
		Where("finished_at IS NOT NULL AND finished_at < ?", before).
		Where(`attempt < (
			SELECT COALESCE(MAX(current_span.attempt), 0)
			FROM custom_processing_spans_v2 AS current_span
			WHERE current_span.knowledge_id = custom_processing_spans_v2.knowledge_id
		)`).
		Where(`attempt <> COALESCE((
			SELECT MAX(success_span.attempt)
			FROM custom_processing_spans_v2 AS success_span
			WHERE success_span.knowledge_id = custom_processing_spans_v2.knowledge_id
			  AND success_span.logical_key = 'root'
			  AND success_span.status = 'done'
		), 0)`).
		Order("finished_at").
		Limit(batch).
		Find(&keys).Error
	if err != nil || len(keys) == 0 {
		return 0, err
	}
	var deleted int64
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SET LOCAL lock_timeout = '500ms'").Error; err != nil {
				return err
			}
			if err := tx.Exec("SET LOCAL statement_timeout = '10s'").Error; err != nil {
				return err
			}
		}
		for _, key := range keys {
			result := tx.Delete(&Span{}, "knowledge_id = ? AND attempt = ? AND logical_key = ?",
				key.KnowledgeID, key.Attempt, key.LogicalKey)
			if result.Error != nil {
				return result.Error
			}
			deleted += result.RowsAffected
		}
		return nil
	})
	if err == nil {
		pipelineobs.ProcessingSpanRetentionDeleted(deleted)
	}
	return deleted, err
}

func (r *Repository) RefreshMetrics(ctx context.Context) error {
	var rows int64
	if err := r.db.WithContext(ctx).Model(&Span{}).Count(&rows).Error; err != nil {
		return err
	}
	pipelineobs.SetProcessingSpanRows(rows)
	return nil
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max]
}
