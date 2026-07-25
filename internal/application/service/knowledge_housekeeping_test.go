package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// knowledgeTestDDL is the minimal subset of the knowledge schema this
// suite needs. We avoid AutoMigrate because Knowledge carries multiple
// JSONB-tagged fields whose SQLite mapping is fragile.
//
// Table name is `knowledges` (plural) — that's what migration 000000
// creates and what GORM's default pluralization expects when the
// service code uses Model(&types.Knowledge{}).
const knowledgeTestDDL = `
CREATE TABLE IF NOT EXISTS knowledges (
    id              VARCHAR(64) PRIMARY KEY,
    tenant_id       INTEGER NOT NULL DEFAULT 0,
    knowledge_base_id VARCHAR(64),
    parse_status    VARCHAR(32) NOT NULL DEFAULT 'pending',
    processing_generation VARCHAR(36) NOT NULL DEFAULT '',
    processing_owner VARCHAR(160) NOT NULL DEFAULT '',
    processing_fanout TEXT,
    summary_status  VARCHAR(32) NOT NULL DEFAULT 'none',
    enrichment_status VARCHAR(32) NOT NULL DEFAULT 'none',
    wiki_status VARCHAR(32) NOT NULL DEFAULT 'none',
    wiki_error_message TEXT NOT NULL DEFAULT '',
    pending_subtasks_count INTEGER NOT NULL DEFAULT 0,
    error_message   TEXT,
    title           TEXT,
    file_type       TEXT,
    enable_status   TEXT NOT NULL DEFAULT 'enabled',
    type            TEXT NOT NULL DEFAULT 'document',
    embedding_model_id TEXT NOT NULL DEFAULT '',
    storage_size    BIGINT NOT NULL DEFAULT 0,
    processed_at    DATETIME,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    deleted_at      DATETIME
);
`

const housekeepingSpansDDL = `
CREATE TABLE IF NOT EXISTS knowledge_processing_spans (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    knowledge_id    VARCHAR(64) NOT NULL,
    attempt         INTEGER     NOT NULL DEFAULT 1,
    span_id         VARCHAR(64) NOT NULL,
    parent_span_id  VARCHAR(64),
    name            VARCHAR(64) NOT NULL,
    kind            VARCHAR(16) NOT NULL,
    status          VARCHAR(16) NOT NULL,
    input           TEXT,
    output          TEXT,
    metadata        TEXT,
    error_code      VARCHAR(64),
    error_message   TEXT,
    error_detail    TEXT,
    started_at      DATETIME,
    finished_at     DATETIME,
    duration_ms     BIGINT,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (knowledge_id, attempt, span_id)
);
`

const housekeepingPendingOpsDDL = `
CREATE TABLE IF NOT EXISTS task_pending_ops (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id   INTEGER NOT NULL DEFAULT 0,
    task_type   VARCHAR(64) NOT NULL,
    scope       VARCHAR(32) NOT NULL,
    scope_id    VARCHAR(64) NOT NULL,
    op          VARCHAR(32) NOT NULL,
    dedup_key   VARCHAR(128) NOT NULL DEFAULT '',
    payload     TEXT NOT NULL DEFAULT '{}',
    fail_count  INTEGER NOT NULL DEFAULT 0,
    enqueued_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    claimed_at  DATETIME
);
`

const housekeepingChunksDDL = `
CREATE TABLE IF NOT EXISTS chunks (
    id              VARCHAR(64) PRIMARY KEY,
    knowledge_id    VARCHAR(64) NOT NULL,
    is_enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    deleted_at      DATETIME
);
`

func setupHousekeepingDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(knowledgeTestDDL).Error)
	require.NoError(t, db.Exec(housekeepingSpansDDL).Error)
	require.NoError(t, db.Exec(housekeepingPendingOpsDDL).Error)
	require.NoError(t, db.Exec(housekeepingChunksDDL).Error)
	return db
}

// insertKnowledge writes a knowledge row at the given updated_at. We
// can't pass updated_at through GORM defaults since CURRENT_TIMESTAMP
// would override our test fixture; raw SQL keeps the timestamp.
func insertKnowledge(t *testing.T, db *gorm.DB, id, status string, updatedAt time.Time) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges (id, knowledge_base_id, parse_status, updated_at) VALUES (?, 'kb-test', ?, ?)`,
		id, status, updatedAt,
	).Error)
}

func insertSpan(t *testing.T, db *gorm.DB, kid string, attempt int, spanID, status string, updatedAt time.Time) {
	insertNamedSpan(t, db, kid, attempt, spanID, "docreader", status, updatedAt)
}

func insertNamedSpan(t *testing.T, db *gorm.DB, kid string, attempt int, spanID, name, status string, updatedAt time.Time) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledge_processing_spans (knowledge_id, attempt, span_id, name, kind, status, updated_at)
		 VALUES (?, ?, ?, ?, 'stage', ?, ?)`,
		kid, attempt, spanID, name, status, updatedAt,
	).Error)
}

func setSummaryProcessing(t *testing.T, db *gorm.DB, id string, updatedAt time.Time) {
	t.Helper()
	require.NoError(t, db.Exec(
		`UPDATE knowledges SET summary_status = ?, updated_at = ? WHERE id = ?`,
		types.SummaryStatusProcessing, updatedAt, id,
	).Error)
}

func markProcessedWithLiveChunk(t *testing.T, db *gorm.DB, id string, processedAt time.Time) {
	t.Helper()
	require.NoError(t, db.Exec(
		`UPDATE knowledges SET processed_at = ?, enable_status = 'enabled' WHERE id = ?`,
		processedAt, id,
	).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO chunks (id, knowledge_id, is_enabled) VALUES (?, ?, TRUE)`,
		"chunk-"+id, id,
	).Error)
}

// fakeTaskInspector is a controllable TaskInspector for the housekeeping
// suite. queued maps knowledge_id → "still has a queued task"; err forces
// the probe to fail so the fail-safe branch can be exercised.
type fakeTaskInspector struct {
	queued        map[string]bool
	err           error
	summaryQueued map[string]bool
	summaryErr    error
}

func (f fakeTaskInspector) CancelTasksForKnowledge(
	_ context.Context, _ string,
) (int, int, error) {
	return 0, 0, nil
}

func (f fakeTaskInspector) HasQueuedTasksForKnowledge(
	_ context.Context, _ string, knowledgeID string,
) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.queued[knowledgeID], nil
}

func (f fakeTaskInspector) QueuedKnowledgeIDs(
	_ context.Context, targets []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	queued := make(map[string]bool)
	for _, target := range targets {
		if f.queued[target.KnowledgeID] {
			queued[target.KnowledgeID] = true
		}
	}
	return queued, nil
}

func (f fakeTaskInspector) DocumentLifecycleTaskKnowledgeIDs(
	ctx context.Context, targets []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	return f.QueuedKnowledgeIDs(ctx, targets)
}

func (f fakeTaskInspector) SummaryTaskKnowledgeIDs(
	_ context.Context, targets []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	if f.summaryErr != nil {
		return nil, f.summaryErr
	}
	queued := make(map[string]bool)
	for _, target := range targets {
		if f.summaryQueued[target.KnowledgeID] {
			queued[target.KnowledgeID] = true
		}
	}
	return queued, nil
}

type queueProbeResult struct {
	queued map[string]bool
	err    error
}

// scriptedTaskInspector models task-state movement between the first and
// second full queue scans. Hooks run during the corresponding probe, before
// the service executes its guarded UPDATE, which makes TOCTOU regressions
// deterministic instead of timing-dependent.
type scriptedTaskInspector struct {
	lifecycleResults []queueProbeResult
	summaryResults   []queueProbeResult
	lifecycleCalls   int
	summaryCalls     int
	onLifecycleProbe func(call int)
	onSummaryProbe   func(call int)
}

func (s *scriptedTaskInspector) CancelTasksForKnowledge(
	_ context.Context, _ string,
) (int, int, error) {
	return 0, 0, nil
}

func (s *scriptedTaskInspector) HasQueuedTasksForKnowledge(
	_ context.Context, _, _ string,
) (bool, error) {
	return false, nil
}

func (s *scriptedTaskInspector) QueuedKnowledgeIDs(
	_ context.Context, _ []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (s *scriptedTaskInspector) DocumentLifecycleTaskKnowledgeIDs(
	_ context.Context, _ []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	s.lifecycleCalls++
	if s.onLifecycleProbe != nil {
		s.onLifecycleProbe(s.lifecycleCalls)
	}
	return scriptedProbeResult(s.lifecycleResults, s.lifecycleCalls)
}

func (s *scriptedTaskInspector) SummaryTaskKnowledgeIDs(
	_ context.Context, _ []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	s.summaryCalls++
	if s.onSummaryProbe != nil {
		s.onSummaryProbe(s.summaryCalls)
	}
	return scriptedProbeResult(s.summaryResults, s.summaryCalls)
}

func scriptedProbeResult(results []queueProbeResult, call int) (map[string]bool, error) {
	if call <= len(results) {
		result := results[call-1]
		return result.queued, result.err
	}
	return map[string]bool{}, nil
}

func newHousekeepingSvcForTest(db *gorm.DB) *HousekeepingService {
	return newHousekeepingSvcWithInspector(db, fakeTaskInspector{})
}

func newHousekeepingSvcWithInspector(db *gorm.DB, inspector interfaces.TaskInspector) *HousekeepingService {
	cfg := &config.Config{KnowledgeBase: &config.KnowledgeBaseConfig{
		// 1h floor + 10min buffer = 70min cutoff. Tight enough to keep
		// the test's relative timestamps in seconds; the production
		// default of 2h+10min is just a constant scale factor.
		DocumentProcessTimeout: 1 * time.Hour,
	}}
	return NewHousekeepingService(db, cfg, inspector, nil)
}

// TestHousekeeping_RecoversAbandoned exercises the happy path: a
// knowledge stuck at "processing" with no recent heartbeat (no spans,
// stale knowledge.updated_at) MUST be flipped to failed.
func TestHousekeeping_RecoversAbandoned(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour) // well past 70min cutoff
	insertKnowledge(t, db, "kid-abandoned", types.ParseStatusProcessing, stale)

	svc.runSweep(context.Background())

	var status, errMsg string
	require.NoError(t, db.Raw(
		`SELECT parse_status, error_message FROM knowledges WHERE id = ?`, "kid-abandoned",
	).Row().Scan(&status, &errMsg))
	assert.Equal(t, types.ParseStatusFailed, status)
	assert.Contains(t, errMsg, "core parsing stuck")
}

func TestHousekeeping_PreservesMoveRecoveryMarker(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-2 * time.Hour)
	insertKnowledge(t, db, "kid-move-recovery", types.ParseStatusProcessing, stale)
	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("id = ?", "kid-move-recovery").
		Updates(map[string]interface{}{
			"error_message": knowledgeMoveRecoveryRequired + "index state uncertain",
			"updated_at":    stale,
		}).Error)

	svc.runSweep(context.Background())

	var got types.Knowledge
	require.NoError(t, db.First(&got, "id = ?", "kid-move-recovery").Error)
	assert.Equal(t, types.ParseStatusProcessing, got.ParseStatus)
	assert.Contains(t, got.ErrorMessage, knowledgeMoveRecoveryRequired)
}

func TestHousekeeping_PreservesMoveWikiPendingMarker(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-2 * time.Hour)
	insertKnowledge(t, db, "kid-move-wiki-pending", types.ParseStatusProcessing, stale)
	marker := knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target")
	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("id = ?", "kid-move-wiki-pending").
		Updates(map[string]interface{}{
			"error_message": marker,
			"updated_at":    stale,
		}).Error)

	svc.runSweep(context.Background())

	var got types.Knowledge
	require.NoError(t, db.First(&got, "id = ?", "kid-move-wiki-pending").Error)
	assert.Equal(t, types.ParseStatusProcessing, got.ParseStatus)
	assert.Equal(t, marker, got.ErrorMessage)
}

func TestHousekeeping_PostIndexProcessingOrphanCompletesInsteadOfFailing(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-post-index-orphan", types.ParseStatusProcessing, stale)
	markProcessedWithLiveChunk(t, db, "kid-post-index-orphan", stale)
	require.NoError(t, db.Exec(
		`UPDATE knowledges
		 SET summary_status = ?, pending_subtasks_count = 1, error_message = 'stale transient error'
		 WHERE id = ?`,
		types.SummaryStatusPending, "kid-post-index-orphan",
	).Error)

	svc.runSweep(context.Background())

	var status, summaryStatus, errorMessage string
	var pending int
	require.NoError(t, db.Raw(
		`SELECT parse_status, summary_status, pending_subtasks_count, COALESCE(error_message, '')
		 FROM knowledges WHERE id = ?`, "kid-post-index-orphan",
	).Row().Scan(&status, &summaryStatus, &pending, &errorMessage))
	assert.Equal(t, types.ParseStatusCompleted, status)
	assert.Equal(t, types.SummaryStatusFailed, summaryStatus)
	assert.Zero(t, pending)
	assert.Empty(t, errorMessage)
}

func TestHousekeeping_PreservesCommittedCoreFanoutIncludingMalformedPlan(t *testing.T) {
	db := setupHousekeepingDB(t)
	old := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "committed-fanout", types.ParseStatusProcessing, old)
	require.NoError(t, db.Exec(
		`UPDATE knowledges
		 SET tenant_id = 7, processing_generation = 'generation-1', processing_owner = '',
		     processing_fanout = '{}', processed_at = ?
		 WHERE id = 'committed-fanout'`,
		old,
	).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO chunks (id, knowledge_id, is_enabled) VALUES ('chunk-fanout', 'committed-fanout', TRUE)`,
	).Error)

	newHousekeepingSvcForTest(db).runSweep(context.Background())

	var row struct {
		ParseStatus      string
		ProcessingFanout string
	}
	require.NoError(t, db.Table("knowledges").Where("id = 'committed-fanout'").Take(&row).Error)
	assert.Equal(t, types.ParseStatusProcessing, row.ParseStatus)
	assert.Equal(t, "{}", row.ProcessingFanout)
}

func TestHousekeeping_FinalUpdateGuardsConcurrentCoreFanoutCommit(t *testing.T) {
	db := setupHousekeepingDB(t)
	old := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "fanout-commit-race", types.ParseStatusProcessing, old)

	// Housekeeping observed this pre-commit snapshot. The core transaction
	// then commits its durable fanout before housekeeping reaches the terminal
	// UPDATE; the write-side predicate must veto both failure and degradation.
	var stale types.Knowledge
	require.NoError(t, db.Where("id = 'fanout-commit-race'").Take(&stale).Error)
	require.NoError(t, db.Exec(
		`UPDATE knowledges
		 SET tenant_id = 7, processing_generation = 'generation-race', processing_owner = '',
		     processing_fanout = '{}', processed_at = ?
		 WHERE id = 'fanout-commit-race'`,
		old,
	).Error)

	svc := newHousekeepingSvcForTest(db)
	svc.recoverStuckKnowledge(
		context.Background(),
		[]types.Knowledge{stale},
		time.Now().Add(-70*time.Minute),
		70*time.Minute,
	)

	var row struct {
		ParseStatus      string
		ProcessingFanout string
	}
	require.NoError(t, db.Table("knowledges").Where("id = 'fanout-commit-race'").Take(&row).Error)
	assert.Equal(t, types.ParseStatusProcessing, row.ParseStatus)
	assert.Equal(t, "{}", row.ProcessingFanout)
}

func TestHousekeeping_ProcessedTimestampWithoutLiveChunkStillFails(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-no-artifact", types.ParseStatusProcessing, stale)
	require.NoError(t, db.Exec(
		`UPDATE knowledges SET processed_at = ? WHERE id = ?`, stale, "kid-no-artifact",
	).Error)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-no-artifact",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFailed, status,
		"processed_at alone must not promote a row with no live searchable artifact")
}

func TestHousekeeping_ProcessingArtifactProbeFailureIsFailClosed(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-artifact-unknown", types.ParseStatusProcessing, stale)
	require.NoError(t, db.Exec(
		`UPDATE knowledges SET processed_at = ? WHERE id = ?`, stale, "kid-artifact-unknown",
	).Error)
	require.NoError(t, db.Exec(`DROP TABLE chunks`).Error)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-artifact-unknown",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusProcessing, status,
		"unknown artifact visibility must preserve a processing candidate")
}

// TestHousekeeping_NoFalseKill_ActiveSpan is the regression test for
// the "long DocReader silently runs longer than DocumentProcessTimeout"
// scenario the user flagged. A knowledge whose knowledge.updated_at
// looks stale BUT whose span tree shows recent activity must NOT be
// killed.
func TestHousekeeping_NoFalseKill_ActiveSpan(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-active", types.ParseStatusProcessing, stale)
	// Span heartbeat well within the 70min cutoff — it represents
	// "we're STILL working, the worker just hasn't transitioned the
	// parse_status column yet".
	insertSpan(t, db, "kid-active", 1, "docreader-1", types.SpanStatusRunning, time.Now().Add(-2*time.Minute))

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-active",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusProcessing, status,
		"knowledge with recent span heartbeat must NOT be flipped to failed")
}

// TestHousekeeping_NoFalseKill_StaleSpanRecovers confirms the inverse:
// a knowledge whose span tree has ALSO gone silent past the threshold
// is genuinely stuck and must be recovered.
func TestHousekeeping_NoFalseKill_StaleSpanRecovers(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-stuck", types.ParseStatusProcessing, stale)
	// Span row stale by the same amount — no recent activity anywhere.
	insertSpan(t, db, "kid-stuck", 1, "docreader-1", types.SpanStatusRunning, stale)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-stuck",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFailed, status,
		"genuinely stuck knowledge (knowledge AND spans both stale) must still be recovered")
}

func TestHousekeeping_RecentWikiSpanDoesNotOwnDocumentLifecycle(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-wiki-heartbeat", types.ParseStatusFinalizing, stale)
	insertNamedSpan(t, db, "kid-wiki-heartbeat", 1, "postprocess-wiki-1",
		"postprocess.wiki.page[example]", types.SpanStatusRunning, time.Now())

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-wiki-heartbeat",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusCompleted, status,
		"independent Wiki activity must not keep a legacy finalizing row stuck")
}

// TestHousekeeping_NoFalseKill_TasksStillQueued is the regression test
// for the backpressure case: a finalizing row whose span heartbeat has
// gone stale (enrichment subtasks fanned out but no worker has picked
// them up yet) must NOT be killed while its tasks are still queued.
func TestHousekeeping_NoFalseKill_TasksStillQueued(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcWithInspector(db, fakeTaskInspector{
		queued: map[string]bool{"kid-backlogged": true},
	})
	stale := time.Now().Add(-3 * time.Hour)
	// finalizing + stale knowledge + stale span: span-only heuristics
	// would flag this as stuck, but the queue still holds its subtasks.
	insertKnowledge(t, db, "kid-backlogged", types.ParseStatusFinalizing, stale)
	insertSpan(t, db, "kid-backlogged", 1, "post-1", types.SpanStatusRunning, stale)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-backlogged",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFinalizing, status,
		"finalizing row with tasks still queued must NOT be flipped to failed")
}

// TestHousekeeping_QueueProbeError_Preserves confirms the fail-closed
// direction: an unavailable queue backend means activity is unknown, not
// absent, so housekeeping must preserve the row and retry later.
func TestHousekeeping_QueueProbeError_Preserves(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcWithInspector(db, fakeTaskInspector{
		err: errors.New("redis unavailable"),
	})
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-probeerr", types.ParseStatusProcessing, stale)

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-probeerr",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusProcessing, status,
		"queue probe error must preserve the row rather than manufacture a parse failure")
}

// TestHousekeeping_FinalizingOrphan_DegradesToCompleted captures the core
// lifecycle invariant: entering finalizing proves the primary parse artifacts
// are already committed. Losing optional enrichment must not turn that usable
// document into a parse failure.
func TestHousekeeping_FinalizingOrphan_DegradesToCompleted(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-finalizing-orphan", types.ParseStatusFinalizing, stale)
	require.NoError(t, db.Exec(
		`UPDATE knowledges
		 SET pending_subtasks_count = 3, summary_status = ?, error_message = 'old transient error', updated_at = ?
		 WHERE id = ?`,
		types.SummaryStatusProcessing, stale, "kid-finalizing-orphan",
	).Error)

	svc.runSweep(context.Background())

	var status, summaryStatus, errMsg string
	var pending int
	var processedAt *time.Time
	require.NoError(t, db.Raw(
		`SELECT parse_status, summary_status, pending_subtasks_count, error_message, processed_at
		 FROM knowledges WHERE id = ?`, "kid-finalizing-orphan",
	).Row().Scan(&status, &summaryStatus, &pending, &errMsg, &processedAt))
	assert.Equal(t, types.ParseStatusCompleted, status)
	assert.Equal(t, types.SummaryStatusFailed, summaryStatus,
		"an abandoned in-progress summary should surface its own degraded status")
	assert.Zero(t, pending)
	assert.Empty(t, errMsg, "optional enrichment must not leave a parse error")
	assert.NotNil(t, processedAt, "completed recovery must stamp terminal processing time")
}

// TestHousekeeping_FinalizingRecovery_RechecksFreshSpan closes the
// candidate-query/update race. A worker heartbeat written after the initial
// candidate was read must make the guarded UPDATE affect zero rows.
func TestHousekeeping_FinalizingRecovery_RechecksFreshSpan(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-raced-span", types.ParseStatusFinalizing, stale)

	var candidate types.Knowledge
	require.NoError(t, db.First(&candidate, "id = ?", "kid-raced-span").Error)
	insertSpan(t, db, candidate.ID, 1, "late-heartbeat", types.SpanStatusRunning, time.Now())

	svc.recoverStuckKnowledge(context.Background(), []types.Knowledge{candidate},
		time.Now().Add(-70*time.Minute), 70*time.Minute)

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, candidate.ID,
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFinalizing, status,
		"a heartbeat arriving between probe and update must veto recovery")
}

// TestHousekeeping_FinalizingRecovery_IgnoresIndependentWikiPending enforces
// the 000056 lifecycle contract: Wiki owns no pending_subtasks_count slot, so
// a large KB-scoped Wiki backlog must not keep a document in finalizing. The
// Wiki op itself remains queued and continues independently.
func TestHousekeeping_FinalizingRecovery_IgnoresIndependentWikiPending(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-raced-wiki", types.ParseStatusFinalizing, stale)

	var candidate types.Knowledge
	require.NoError(t, db.First(&candidate, "id = ?", "kid-raced-wiki").Error)
	require.NoError(t, db.Exec(
		`INSERT INTO task_pending_ops
		 (tenant_id, task_type, scope, scope_id, op, dedup_key, payload)
		 VALUES (1, ?, ?, ?, 'ingest', ?, '{}')`,
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
		candidate.KnowledgeBaseID, candidate.ID,
	).Error)

	cutoff := time.Now().Add(-70 * time.Minute)
	svc.recoverStuckKnowledge(context.Background(), []types.Knowledge{candidate}, cutoff, 70*time.Minute)

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, candidate.ID,
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusCompleted, status,
		"independent Wiki backlog must not own or block document finalization")

	var pendingCount int64
	require.NoError(t, db.Table("task_pending_ops").
		Where("dedup_key = ?", candidate.ID).
		Count(&pendingCount).Error)
	assert.Equal(t, int64(1), pendingCount,
		"document recovery must not consume or mutate the independent Wiki queue")
}

func TestHousekeeping_ProcessingRecovery_IgnoresIndependentWikiPending(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-processing-with-wiki", types.ParseStatusProcessing, stale)

	var candidate types.Knowledge
	require.NoError(t, db.First(&candidate, "id = ?", "kid-processing-with-wiki").Error)
	require.NoError(t, db.Exec(
		`INSERT INTO task_pending_ops
		 (tenant_id, task_type, scope, scope_id, op, dedup_key, payload)
		 VALUES (1, ?, ?, ?, 'ingest', ?, '{}')`,
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
		candidate.KnowledgeBaseID, candidate.ID,
	).Error)

	svc.recoverStuckKnowledge(context.Background(), []types.Knowledge{candidate},
		time.Now().Add(-70*time.Minute), 70*time.Minute)

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, candidate.ID,
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFailed, status,
		"an unrelated Wiki op must not conceal an abandoned core parse")

	var pendingCount int64
	require.NoError(t, db.Table("task_pending_ops").
		Where("dedup_key = ?", candidate.ID).
		Count(&pendingCount).Error)
	assert.Equal(t, int64(1), pendingCount,
		"core recovery must leave independent Wiki queue ownership untouched")
}

// TestHousekeeping_PreservesRecentlyTouched: any knowledge whose
// updated_at is within the cutoff is left alone — that's the cheap
// fast path that doesn't even consult the spans table.
func TestHousekeeping_PreservesRecentlyTouched(t *testing.T) {
	db := setupHousekeepingDB(t)
	svc := newHousekeepingSvcForTest(db)
	insertKnowledge(t, db, "kid-fresh", types.ParseStatusProcessing, time.Now().Add(-30*time.Second))

	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-fresh",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusProcessing, status,
		"knowledge updated within the cutoff must be left alone")
}

// TestHousekeeping_SecondProbeCatchesActiveToRetryTransition models the
// narrow hole in a non-atomic Asynq scan: the first pass misses a task while
// it moves out of active, then the complete second pass observes it in retry.
// The document must remain non-terminal.
func TestHousekeeping_SecondProbeCatchesActiveToRetryTransition(t *testing.T) {
	db := setupHousekeepingDB(t)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-active-to-retry", types.ParseStatusProcessing, stale)

	inspector := &scriptedTaskInspector{lifecycleResults: []queueProbeResult{
		{queued: map[string]bool{}},
		{queued: map[string]bool{"kid-active-to-retry": true}},
	}}
	svc := newHousekeepingSvcWithInspector(db, inspector)
	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-active-to-retry",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusProcessing, status)
	assert.Equal(t, 2, inspector.lifecycleCalls,
		"an apparent orphan must receive two complete liveness probes")
}

func TestHousekeeping_SecondProbeErrorPreserves(t *testing.T) {
	db := setupHousekeepingDB(t)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-second-probe-error", types.ParseStatusFinalizing, stale)

	inspector := &scriptedTaskInspector{lifecycleResults: []queueProbeResult{
		{queued: map[string]bool{}},
		{err: errors.New("redis changed master during final probe")},
	}}
	svc := newHousekeepingSvcWithInspector(db, inspector)
	svc.runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-second-probe-error",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusFinalizing, status,
		"unknown liveness on the final probe must veto the write")
	assert.Equal(t, 2, inspector.lifecycleCalls)
}

func TestHousekeeping_FinalUpdateGuardsKnowledgeRace(t *testing.T) {
	tests := []struct {
		name       string
		lateUpdate func(t *testing.T, db *gorm.DB, stale time.Time)
		wantStatus string
	}{
		{
			name: "knowledge heartbeat advances",
			lateUpdate: func(t *testing.T, db *gorm.DB, _ time.Time) {
				require.NoError(t, db.Exec(
					`UPDATE knowledges SET updated_at = ? WHERE id = ?`,
					time.Now(), "kid-lifecycle-race",
				).Error)
			},
			wantStatus: types.ParseStatusProcessing,
		},
		{
			name: "pipeline reaches terminal state",
			lateUpdate: func(t *testing.T, db *gorm.DB, stale time.Time) {
				require.NoError(t, db.Exec(
					`UPDATE knowledges SET parse_status = ?, updated_at = ? WHERE id = ?`,
					types.ParseStatusCompleted, stale, "kid-lifecycle-race",
				).Error)
			},
			wantStatus: types.ParseStatusCompleted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupHousekeepingDB(t)
			stale := time.Now().Add(-3 * time.Hour)
			insertKnowledge(t, db, "kid-lifecycle-race", types.ParseStatusProcessing, stale)

			inspector := &scriptedTaskInspector{
				lifecycleResults: []queueProbeResult{
					{queued: map[string]bool{}},
					{queued: map[string]bool{}},
				},
				onLifecycleProbe: func(call int) {
					if call == 2 {
						tt.lateUpdate(t, db, stale)
					}
				},
			}
			newHousekeepingSvcWithInspector(db, inspector).runSweep(context.Background())

			var status string
			require.NoError(t, db.Raw(
				`SELECT parse_status FROM knowledges WHERE id = ?`, "kid-lifecycle-race",
			).Row().Scan(&status))
			assert.Equal(t, tt.wantStatus, status)
			assert.Equal(t, 2, inspector.lifecycleCalls)
		})
	}
}

func TestHousekeeping_SummaryLiveTaskPreserves(t *testing.T) {
	tests := []struct {
		name    string
		results []queueProbeResult
		calls   int
	}{
		{
			name: "queued on first scan",
			results: []queueProbeResult{
				{queued: map[string]bool{"kid-summary-live": true}},
			},
			calls: 1,
		},
		{
			name: "becomes active before final scan",
			results: []queueProbeResult{
				{queued: map[string]bool{}},
				{queued: map[string]bool{"kid-summary-live": true}},
			},
			calls: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupHousekeepingDB(t)
			stale := time.Now().Add(-3 * time.Hour)
			insertKnowledge(t, db, "kid-summary-live", types.ParseStatusCompleted, stale)
			setSummaryProcessing(t, db, "kid-summary-live", stale)

			inspector := &scriptedTaskInspector{summaryResults: tt.results}
			svc := newHousekeepingSvcWithInspector(db, inspector)
			svc.runSweep(context.Background())

			var status string
			require.NoError(t, db.Raw(
				`SELECT summary_status FROM knowledges WHERE id = ?`, "kid-summary-live",
			).Row().Scan(&status))
			assert.Equal(t, types.SummaryStatusProcessing, status)
			assert.Equal(t, tt.calls, inspector.summaryCalls)
		})
	}
}

func TestHousekeeping_SummaryProbeErrorPreserves(t *testing.T) {
	tests := []struct {
		name    string
		results []queueProbeResult
		calls   int
	}{
		{
			name: "first probe error",
			results: []queueProbeResult{
				{err: errors.New("redis unavailable")},
			},
			calls: 1,
		},
		{
			name: "second probe error",
			results: []queueProbeResult{
				{queued: map[string]bool{}},
				{err: errors.New("redis unavailable")},
			},
			calls: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupHousekeepingDB(t)
			stale := time.Now().Add(-3 * time.Hour)
			insertKnowledge(t, db, "kid-summary-error", types.ParseStatusCompleted, stale)
			setSummaryProcessing(t, db, "kid-summary-error", stale)

			inspector := &scriptedTaskInspector{summaryResults: tt.results}
			newHousekeepingSvcWithInspector(db, inspector).runSweep(context.Background())

			var status string
			require.NoError(t, db.Raw(
				`SELECT summary_status FROM knowledges WHERE id = ?`, "kid-summary-error",
			).Row().Scan(&status))
			assert.Equal(t, types.SummaryStatusProcessing, status)
			assert.Equal(t, tt.calls, inspector.summaryCalls)
		})
	}
}

func TestHousekeeping_SummaryFinalUpdateGuardsRace(t *testing.T) {
	tests := []struct {
		name       string
		lateUpdate func(t *testing.T, db *gorm.DB, stale time.Time)
		wantStatus string
	}{
		{
			name: "updated_at heartbeat",
			lateUpdate: func(t *testing.T, db *gorm.DB, _ time.Time) {
				require.NoError(t, db.Exec(
					`UPDATE knowledges SET updated_at = ? WHERE id = ?`,
					time.Now(), "kid-summary-race",
				).Error)
			},
			wantStatus: types.SummaryStatusProcessing,
		},
		{
			name: "task completes",
			lateUpdate: func(t *testing.T, db *gorm.DB, stale time.Time) {
				require.NoError(t, db.Exec(
					`UPDATE knowledges SET summary_status = ?, updated_at = ? WHERE id = ?`,
					types.SummaryStatusCompleted, stale, "kid-summary-race",
				).Error)
			},
			wantStatus: types.SummaryStatusCompleted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupHousekeepingDB(t)
			stale := time.Now().Add(-3 * time.Hour)
			insertKnowledge(t, db, "kid-summary-race", types.ParseStatusCompleted, stale)
			setSummaryProcessing(t, db, "kid-summary-race", stale)

			inspector := &scriptedTaskInspector{
				summaryResults: []queueProbeResult{
					{queued: map[string]bool{}},
					{queued: map[string]bool{}},
				},
				onSummaryProbe: func(call int) {
					if call == 2 {
						tt.lateUpdate(t, db, stale)
					}
				},
			}
			newHousekeepingSvcWithInspector(db, inspector).runSweep(context.Background())

			var status string
			require.NoError(t, db.Raw(
				`SELECT summary_status FROM knowledges WHERE id = ?`, "kid-summary-race",
			).Row().Scan(&status))
			assert.Equal(t, tt.wantStatus, status,
				"late state progress must make the guarded UPDATE affect zero rows")
			assert.Equal(t, 2, inspector.summaryCalls)
		})
	}
}

func TestHousekeeping_SummaryOrphanRecoversAfterTwoProbes(t *testing.T) {
	db := setupHousekeepingDB(t)
	stale := time.Now().Add(-3 * time.Hour)
	insertKnowledge(t, db, "kid-summary-orphan", types.ParseStatusCompleted, stale)
	setSummaryProcessing(t, db, "kid-summary-orphan", stale)

	inspector := &scriptedTaskInspector{summaryResults: []queueProbeResult{
		{queued: map[string]bool{}},
		{queued: map[string]bool{}},
	}}
	newHousekeepingSvcWithInspector(db, inspector).runSweep(context.Background())

	var status string
	require.NoError(t, db.Raw(
		`SELECT summary_status FROM knowledges WHERE id = ?`, "kid-summary-orphan",
	).Row().Scan(&status))
	assert.Equal(t, types.SummaryStatusFailed, status)
	assert.Equal(t, 2, inspector.summaryCalls)
}
