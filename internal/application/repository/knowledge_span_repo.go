package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/processingtrace"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// KnowledgeSpanRepository persists the per-attempt span tree used by the
// processing pipeline. Operations are deliberately narrow:
//
//   - Upsert covers Begin/End/Fail/Skip — every state transition routes
//     through the same write so the row stays internally consistent.
//   - NextAttempt allocates a new attempt for re-parses without touching
//     historical rows. Old attempts stay queryable for post-mortem.
//   - ListByAttempt is the only read path; the handler builds the tree
//     in memory rather than recursing through the DB.
type KnowledgeSpanRepository interface {
	Upsert(ctx context.Context, row *types.KnowledgeProcessingSpan) error
	// CreateNextAttemptRoot atomically allocates the next per-document
	// attempt, terminalizes every older pending/running span, and inserts the
	// new root. PostgreSQL callers are serialized by a transaction-scoped
	// advisory lock, so two application pods cannot allocate the same attempt.
	// The returned count is the number of older open rows superseded.
	CreateNextAttemptRoot(
		ctx context.Context,
		root *types.KnowledgeProcessingSpan,
		supersedeErrorCode string,
		supersedeReason string,
	) (attempt int, superseded int64, err error)
	NextAttempt(ctx context.Context, knowledgeID string) (int, error)
	LatestAttempt(ctx context.Context, knowledgeID string) (int, error)
	ListByAttempt(ctx context.Context, knowledgeID string, attempt int) ([]types.KnowledgeProcessingSpan, error)
	GetSpan(ctx context.Context, knowledgeID string, attempt int, spanID string) (*types.KnowledgeProcessingSpan, error)
	// CancelDescendants marks every descendant of a parent span as
	// "cancelled" with the given reason. Used by the tracker to
	// cascade an upstream failure across a stage's downstream subtree
	// without iterating in Go memory.
	CancelDescendants(ctx context.Context, knowledgeID string, attempt int, parentSpanID, reason string) (int64, error)
	// CancelAllOpenSpans flips every non-terminal (pending/running) span
	// for (knowledgeID, attempt) to "cancelled" in one statement,
	// regardless of tree position. Used by the user-cancel path where
	// fan-out stages (e.g. "多模态识别") flip themselves to done as soon
	// as they finish dispatching, while their async children are still
	// running — a tree walk that stops at terminal parents would miss
	// those orphan leaves.
	CancelAllOpenSpans(ctx context.Context, knowledgeID string, attempt int, errorCode, reason string) (int64, error)
}

type knowledgeSpanRepository struct {
	db *gorm.DB
	v2 *processingtrace.Repository
	// SQLite cannot be shared by horizontally scaled pods and has no
	// transaction-scoped advisory lock. Serializing allocation inside the
	// single process gives its supported local mode the same invariant.
	sqliteAttemptMu sync.Mutex
}

// NewKnowledgeSpanRepositoryWithV2 wires the sole authoritative span store.
// db is retained only to identify SQLite's single-process test/local mode;
// every span read and write goes through the V2 repository.
func NewKnowledgeSpanRepositoryWithV2(
	db *gorm.DB,
	v2 *processingtrace.Repository,
) KnowledgeSpanRepository {
	return &knowledgeSpanRepository{db: db, v2: v2}
}

func (r *knowledgeSpanRepository) Upsert(ctx context.Context, row *types.KnowledgeProcessingSpan) error {
	if row == nil || row.KnowledgeID == "" || row.SpanID == "" {
		return errors.New("knowledgeSpanRepository.Upsert: knowledge_id and span_id required")
	}
	if row.Attempt == 0 {
		row.Attempt = 1
	}
	if r == nil || r.v2 == nil {
		return errors.New("knowledgeSpanRepository.Upsert: V2 repository required")
	}
	return r.upsertV2(ctx, row)
}

func (r *knowledgeSpanRepository) upsertV2(
	ctx context.Context,
	row *types.KnowledgeProcessingSpan,
) error {
	if r == nil || r.v2 == nil || row == nil {
		return errors.New("knowledgeSpanRepository.upsertV2: V2 repository required")
	}
	logicalKey := spanV2LogicalKey(row.Kind, row.Name)
	row.SpanID = processingtrace.DeterministicSpanID(row.KnowledgeID, row.Attempt, logicalKey)
	parentKey := ""
	if row.ParentSpanID != "" {
		if parent, err := r.v2.GetBySpanID(ctx, row.KnowledgeID, row.Attempt, row.ParentSpanID); err == nil && parent != nil {
			parentKey = parent.LogicalKey
		}
	}
	summary := func(value types.JSONMap) string {
		if value == nil {
			return ""
		}
		raw, _ := json.Marshal(value)
		return string(raw)
	}
	started := row.CreatedAt
	if row.StartedAt != nil {
		started = *row.StartedAt
	}
	var progressAt *time.Time
	if row.Status == types.SpanStatusDone || row.Status == types.SpanStatusFailed ||
		row.Status == types.SpanStatusSkipped || row.Status == types.SpanStatusCancelled {
		at := row.UpdatedAt
		if at.IsZero() {
			at = time.Now()
		}
		progressAt = &at
	}
	return r.v2.RecordBusinessProgress(ctx, processingtrace.Upsert{
		KnowledgeID: row.KnowledgeID, Attempt: row.Attempt,
		LogicalKey: logicalKey, ParentLogicalKey: parentKey,
		Name: row.Name, Kind: row.Kind, Status: row.Status,
		InputSummary: summary(row.Input), OutputSummary: summary(row.Output),
		MetadataSummary: summary(row.Metadata),
		LastErrorCode:   row.ErrorCode, LastErrorMessage: row.ErrorMessage,
		LastErrorDetail: row.ErrorDetail,
		StartedAt:       started, LastBusinessProgressAt: progressAt,
		FinishedAt:           row.FinishedAt,
		IncrementRealAttempt: row.Status == types.SpanStatusRunning,
	})
}

func spanV2LogicalKey(kind, name string) string {
	return processingtrace.LogicalKey(kind, name)
}

func (r *knowledgeSpanRepository) CreateNextAttemptRoot(
	ctx context.Context,
	root *types.KnowledgeProcessingSpan,
	supersedeErrorCode string,
	supersedeReason string,
) (attempt int, superseded int64, err error) {
	if root == nil || root.KnowledgeID == "" || root.SpanID == "" {
		return 0, 0, errors.New(
			"knowledgeSpanRepository.CreateNextAttemptRoot: knowledge_id and span_id required",
		)
	}
	if root.Kind != types.SpanKindRoot {
		return 0, 0, fmt.Errorf(
			"knowledgeSpanRepository.CreateNextAttemptRoot: kind %q is not root",
			root.Kind,
		)
	}
	if supersedeErrorCode == "" {
		supersedeErrorCode = "SUPERSEDED_ATTEMPT"
	}
	if supersedeReason == "" {
		supersedeReason = "a newer document-processing attempt was accepted"
	}

	isSQLite := r.db != nil && r.db.Dialector != nil &&
		r.db.Dialector.Name() == "sqlite"
	if isSQLite {
		r.sqliteAttemptMu.Lock()
		defer r.sqliteAttemptMu.Unlock()
	}
	if r == nil || r.v2 == nil {
		return 0, 0, errors.New("knowledgeSpanRepository.CreateNextAttemptRoot: V2 repository required")
	}
	summary := func(value types.JSONMap) string {
		if value == nil {
			return ""
		}
		raw, _ := json.Marshal(value)
		return string(raw)
	}
	started := time.Now()
	if root.StartedAt != nil {
		started = *root.StartedAt
	}
	created, count, allocateErr := r.v2.AllocateAttemptRoot(ctx, processingtrace.Upsert{
		KnowledgeID: root.KnowledgeID, LogicalKey: "root", Name: root.Name,
		Kind: root.Kind, Status: root.Status, InputSummary: summary(root.Input),
		OutputSummary: summary(root.Output), MetadataSummary: summary(root.Metadata),
		LastErrorCode: root.ErrorCode, LastErrorMessage: root.ErrorMessage,
		LastErrorDetail: root.ErrorDetail, StartedAt: started, FinishedAt: root.FinishedAt,
	}, supersedeErrorCode, supersedeReason)
	if allocateErr != nil {
		return 0, 0, allocateErr
	}
	root.Attempt = created.Attempt
	root.SpanID = created.SpanID
	root.CreatedAt = created.CreatedAt
	root.UpdatedAt = created.UpdatedAt
	return created.Attempt, count, nil
}

func (r *knowledgeSpanRepository) NextAttempt(ctx context.Context, knowledgeID string) (int, error) {
	latest, err := r.v2.LatestAttempt(ctx, knowledgeID)
	return latest + 1, err
}

func (r *knowledgeSpanRepository) LatestAttempt(ctx context.Context, knowledgeID string) (int, error) {
	return r.v2.LatestAttempt(ctx, knowledgeID)
}

func (r *knowledgeSpanRepository) ListByAttempt(ctx context.Context, knowledgeID string, attempt int) ([]types.KnowledgeProcessingSpan, error) {
	if knowledgeID == "" {
		return nil, errors.New("knowledgeSpanRepository.ListByAttempt: knowledge_id required")
	}
	if attempt < 1 {
		return nil, errors.New("knowledgeSpanRepository.ListByAttempt: positive attempt required")
	}
	page, err := r.v2.List(ctx, knowledgeID, attempt, processingtrace.MaxPageSize, nil)
	if err != nil {
		return nil, err
	}
	return spanDTOsFromV2(page.Items), nil
}

func (r *knowledgeSpanRepository) GetSpan(ctx context.Context, knowledgeID string, attempt int, spanID string) (*types.KnowledgeProcessingSpan, error) {
	row, err := r.v2.GetBySpanID(ctx, knowledgeID, attempt, spanID)
	if err != nil || row == nil {
		return nil, err
	}
	converted := spanDTOFromV2(*row)
	if row.ParentLogicalKey != "" {
		converted.ParentSpanID = processingtrace.DeterministicSpanID(knowledgeID, attempt, row.ParentLogicalKey)
	}
	return &converted, nil
}

func spanDTOsFromV2(spans []processingtrace.Span) []types.KnowledgeProcessingSpan {
	rows := make([]types.KnowledgeProcessingSpan, 0, len(spans))
	for _, span := range spans {
		row := spanDTOFromV2(span)
		if span.ParentLogicalKey != "" {
			row.ParentSpanID = processingtrace.DeterministicSpanID(span.KnowledgeID, span.Attempt, span.ParentLogicalKey)
		}
		rows = append(rows, row)
	}
	return rows
}

func spanDTOFromV2(span processingtrace.Span) types.KnowledgeProcessingSpan {
	started := span.StartedAt
	row := types.KnowledgeProcessingSpan{
		KnowledgeID: span.KnowledgeID, Attempt: span.Attempt, SpanID: span.SpanID,
		Name: span.Name, Kind: span.Kind, Status: span.Status,
		ErrorCode: span.LastErrorCode, ErrorMessage: span.LastErrorMessage,
		ErrorDetail: span.LastErrorDetail, StartedAt: &started, FinishedAt: span.FinishedAt,
		DurationMs: span.DurationMS, CreatedAt: span.CreatedAt, UpdatedAt: span.UpdatedAt,
	}
	decode := func(raw string, target *types.JSONMap) {
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), target)
		}
	}
	decode(span.InputSummary, &row.Input)
	decode(span.OutputSummary, &row.Output)
	decode(span.MetadataSummary, &row.Metadata)
	return row
}

// CancelDescendants walks stable logical parent keys in memory, then performs
// one set-based update. Pagination avoids silently omitting a large document's
// descendants when it has more than one API page of logical spans.
func (r *knowledgeSpanRepository) CancelDescendants(ctx context.Context, knowledgeID string, attempt int, parentSpanID, reason string) (int64, error) {
	parent, err := r.v2.GetBySpanID(ctx, knowledgeID, attempt, parentSpanID)
	if err != nil || parent == nil {
		return 0, err
	}
	all := make([]processingtrace.Span, 0, processingtrace.MaxPageSize)
	var cursor *processingtrace.Cursor
	for {
		page, listErr := r.v2.List(ctx, knowledgeID, attempt, processingtrace.MaxPageSize, cursor)
		if listErr != nil {
			return 0, listErr
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	frontier := map[string]struct{}{parent.LogicalKey: {}}
	keys := make([]string, 0)
	for depth := 0; depth < 32 && len(frontier) > 0; depth++ {
		next := make(map[string]struct{})
		for _, span := range all {
			if _, ok := frontier[span.ParentLogicalKey]; !ok {
				continue
			}
			keys = append(keys, span.LogicalKey)
			next[span.LogicalKey] = struct{}{}
		}
		frontier = next
	}
	if len(keys) == 0 {
		return 0, nil
	}
	return r.v2.CancelOpen(ctx, knowledgeID, attempt, keys, "UPSTREAM_FAILED", reason)
}

// CancelAllOpenSpans is the "abort the attempt" counterpart to
// CancelDescendants. It avoids the BFS entirely so spans whose parent
// is already terminal (typical for stage fan-outs that EndSpan as soon
// as they finish dispatching async work) still get flipped to cancelled.
// We deliberately do NOT touch finished_at / duration_ms here — the
// span row remains observable in the trace tree with its original
// start time and gets a cancelled status + reason, which is enough
// for the UI to drop the running-bar styling.
func (r *knowledgeSpanRepository) CancelAllOpenSpans(
	ctx context.Context, knowledgeID string, attempt int, errorCode, reason string,
) (int64, error) {
	return r.v2.CancelOpen(ctx, knowledgeID, attempt, nil, errorCode, reason)
}
