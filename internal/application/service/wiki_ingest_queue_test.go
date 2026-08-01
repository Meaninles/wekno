package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiingestguard"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/workretry"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type wikiQueuePendingRepoStub struct {
	interfaces.TaskPendingOpsRepository

	enqueueErr    error
	peekErr       error
	deleteErr     error
	archiveErr    error
	incrErr       error
	countErr      error
	checkpointErr error
	touchErr      error

	rows          []*types.TaskPendingOp
	pendingCount  int64
	countFromRows bool
	incrCount     int

	enqueued        []*types.TaskPendingOp
	deletedIDs      [][]int64
	incrementedIDs  []int64
	archivedIDs     []int64
	archived        []*types.TaskDeadLetter
	enqueueCtxErr   error
	peekCtxErr      error
	deleteCtxErr    error
	incrCtxErr      error
	countCtxErr     error
	peekCalls       int
	deleteCalls     int
	countCalls      int
	checkpointCalls int
	touchedIDs      []int64
	leaseMu         sync.Mutex
	leaseEpoch      int64
	leaseErr        error
	leaseAcquires   int
}

// wikiQueueNoLeasePendingRepo intentionally hides the production lease
// extension while preserving the generic queue interface.
type wikiQueueNoLeasePendingRepo struct {
	interfaces.TaskPendingOpsRepository
}

type wikiDistributedPendingRepoStub struct {
	*wikiQueuePendingRepoStub
}

func (r *wikiDistributedPendingRepoStub) GetWikiIngestByDedupKey(
	_ context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	dedupKey string,
) (*types.TaskPendingOp, error) {
	for _, row := range r.rows {
		if row != nil && row.TenantID == tenantID && row.ScopeID == knowledgeBaseID &&
			row.TaskType == types.TypeWikiIngest && row.Op == WikiOpIngest &&
			row.DedupKey == dedupKey {
			copyRow := *row
			copyRow.Payload = slices.Clone(row.Payload)
			return &copyRow, nil
		}
	}
	return nil, nil
}

func (r *wikiDistributedPendingRepoStub) DeleteWikiIngestByDedupKey(
	_ context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	dedupKey string,
) error {
	kept := r.rows[:0]
	for _, row := range r.rows {
		if row != nil && row.TenantID == tenantID && row.ScopeID == knowledgeBaseID &&
			row.TaskType == types.TypeWikiIngest && row.Op == WikiOpIngest &&
			row.DedupKey == dedupKey {
			continue
		}
		kept = append(kept, row)
	}
	r.rows = kept
	return nil
}

func (r *wikiDistributedPendingRepoStub) PeekWikiCommitBatch(
	_ context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	limit int,
) ([]*types.TaskPendingOp, error) {
	rows := make([]*types.TaskPendingOp, 0)
	for _, row := range r.rows {
		if row == nil || row.TenantID != tenantID || row.ScopeID != knowledgeBaseID {
			continue
		}
		if row.Op == WikiOpIngest && row.MapReadyAt == nil {
			continue
		}
		rows = append(rows, row)
		if limit > 0 && len(rows) == limit {
			break
		}
	}
	return rows, nil
}

func (r *wikiDistributedPendingRepoStub) CountWikiCommitReady(
	_ context.Context,
	tenantID uint64,
	knowledgeBaseID string,
) (int64, error) {
	rows, err := r.PeekWikiCommitBatch(context.Background(), tenantID, knowledgeBaseID, 0)
	return int64(len(rows)), err
}

func (r *wikiDistributedPendingRepoStub) MarkWikiMapReady(
	_ context.Context,
	id int64,
	_ uint64,
	_ string,
	payload []byte,
) (bool, error) {
	r.checkpointCalls++
	if r.checkpointErr != nil {
		return false, r.checkpointErr
	}
	for _, row := range r.rows {
		if row != nil && row.ID == id {
			now := time.Now()
			row.Payload = slices.Clone(payload)
			row.MapReadyAt = &now
			return true, nil
		}
	}
	return false, nil
}

func (r *wikiQueuePendingRepoStub) AcquireWikiIngestLease(
	_ context.Context,
	tenantID uint64,
	knowledgeBaseID string,
) (wikilease.Identity, error) {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	r.leaseAcquires++
	if r.leaseErr != nil {
		return wikilease.Identity{}, r.leaseErr
	}
	r.leaseEpoch++
	return wikilease.Identity{
		TenantID: tenantID, KnowledgeBaseID: knowledgeBaseID,
		Epoch: r.leaseEpoch, Token: fmt.Sprintf("test-wiki-lease-token-%032d", r.leaseEpoch),
	}, nil
}

func (r *wikiQueuePendingRepoStub) WithActiveWikiKnowledgeBase(
	ctx context.Context,
	_ uint64,
	_ string,
	publish func() error,
) error {
	if wikilease.Required(ctx) {
		if _, ok := wikilease.IdentityFromContext(ctx); !ok {
			return wikilease.ErrLeaseRequired
		}
	}
	return publish()
}

func (r *wikiQueuePendingRepoStub) UpdateWikiPayload(
	_ context.Context,
	id int64,
	_ uint64,
	_ string,
	payload []byte,
) (bool, error) {
	r.checkpointCalls++
	if r.checkpointErr != nil {
		return false, r.checkpointErr
	}
	for _, row := range r.rows {
		if row != nil && row.ID == id {
			row.Payload = slices.Clone(payload)
			return true, nil
		}
	}
	return false, nil
}

func (r *wikiQueuePendingRepoStub) Enqueue(ctx context.Context, op *types.TaskPendingOp) error {
	r.enqueueCtxErr = ctx.Err()
	if r.enqueueErr != nil {
		return r.enqueueErr
	}
	copyOp := *op
	copyOp.Payload = slices.Clone(op.Payload)
	r.enqueued = append(r.enqueued, &copyOp)
	return nil
}

func (r *wikiQueuePendingRepoStub) PeekBatch(
	ctx context.Context,
	_, _, _ string,
	limit int,
) ([]*types.TaskPendingOp, error) {
	r.peekCalls++
	r.peekCtxErr = ctx.Err()
	if r.peekErr != nil {
		return nil, r.peekErr
	}
	if limit > 0 && len(r.rows) > limit {
		return r.rows[:limit], nil
	}
	return r.rows, nil
}

func (r *wikiQueuePendingRepoStub) DeleteByIDs(ctx context.Context, ids []int64) error {
	r.deleteCalls++
	r.deleteCtxErr = ctx.Err()
	r.deletedIDs = append(r.deletedIDs, slices.Clone(ids))
	if r.deleteErr != nil {
		return r.deleteErr
	}
	if len(r.rows) > 0 {
		deleted := make(map[int64]struct{}, len(ids))
		for _, id := range ids {
			deleted[id] = struct{}{}
		}
		kept := r.rows[:0]
		for _, row := range r.rows {
			if _, ok := deleted[row.ID]; !ok {
				kept = append(kept, row)
			}
		}
		r.rows = kept
	}
	return nil
}

func (r *wikiQueuePendingRepoStub) ArchiveToDeadLetter(ctx context.Context, id int64, row *types.TaskDeadLetter) error {
	if r.archiveErr != nil {
		return r.archiveErr
	}
	copyRow := *row
	copyRow.Payload = slices.Clone(row.Payload)
	r.archivedIDs = append(r.archivedIDs, id)
	r.archived = append(r.archived, &copyRow)
	if len(r.rows) > 0 {
		kept := r.rows[:0]
		for _, pending := range r.rows {
			if pending.ID != id {
				kept = append(kept, pending)
			}
		}
		r.rows = kept
	}
	return nil
}

func (r *wikiQueuePendingRepoStub) IncrFailCount(ctx context.Context, id int64) (int, error) {
	r.incrCtxErr = ctx.Err()
	r.incrementedIDs = append(r.incrementedIDs, id)
	if r.incrErr != nil {
		return 0, r.incrErr
	}
	return r.incrCount, nil
}

func (r *wikiQueuePendingRepoStub) TouchWikiAttempt(_ context.Context, id int64) error {
	r.touchedIDs = append(r.touchedIDs, id)
	return r.touchErr
}

func (r *wikiQueuePendingRepoStub) PendingCount(ctx context.Context, _, _, _ string) (int64, error) {
	r.countCalls++
	r.countCtxErr = ctx.Err()
	if r.countErr != nil {
		return 0, r.countErr
	}
	if r.countFromRows {
		return int64(len(r.rows)), nil
	}
	return r.pendingCount, nil
}

func TestProcessWikiIngestFailsClosedWithoutDatabaseLeaseRepositoryAndReleasesLiteLock(t *testing.T) {
	generic := &wikiQueuePendingRepoStub{}
	svc := &wikiIngestService{
		pendingRepo: wikiQueueNoLeasePendingRepo{TaskPendingOpsRepository: generic},
	}
	payload, err := json.Marshal(WikiIngestPayload{TenantID: 7, KnowledgeBaseID: "kb-1"})
	require.NoError(t, err)
	err = svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	require.ErrorContains(t, err, "mandatory database lease fencing")
	_, held := svc.liteLocks.Load("kb-1")
	require.False(t, held, "failed DB acquisition must release the Lite coordination lock")
}

func TestProcessWikiIngestDatabaseLeaseAcquireFailureReleasesLiteLockAndRetries(t *testing.T) {
	dbErr := errors.New("database unavailable")
	pending := &wikiQueuePendingRepoStub{leaseErr: dbErr}
	svc := &wikiIngestService{pendingRepo: pending}
	payload, err := json.Marshal(WikiIngestPayload{TenantID: 7, KnowledgeBaseID: "kb-1"})
	require.NoError(t, err)
	err = svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	require.ErrorIs(t, err, dbErr)
	require.Equal(t, 1, pending.leaseAcquires)
	_, held := svc.liteLocks.Load("kb-1")
	require.False(t, held, "DB errors must release the process lock before Asynq retries")
}

func TestProcessWikiIngestDatabaseLeaseAcquireFailureReleasesRedisLock(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("set WEKNORA_TEST_REDIS_ADDR to run the Redis lease-release integration test")
	}
	password := os.Getenv("WEKNORA_TEST_REDIS_PASSWORD")
	if password == "" {
		password = os.Getenv("REDIS_PASSWORD")
	}
	client := redis.NewClient(&redis.Options{Addr: address, Password: password})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.NoError(t, client.Ping(context.Background()).Err())

	kbID := fmt.Sprintf("kb-db-lease-failure-%d", time.Now().UnixNano())
	key := wikiActiveKeyPrefix + kbID
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })
	dbErr := errors.New("database unavailable after Redis lock")
	pending := &wikiQueuePendingRepoStub{leaseErr: dbErr}
	svc := &wikiIngestService{pendingRepo: pending, redisClient: client}
	payload, err := json.Marshal(WikiIngestPayload{TenantID: 7, KnowledgeBaseID: kbID})
	require.NoError(t, err)
	err = svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	require.ErrorIs(t, err, dbErr)
	exists, err := client.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "DB acquisition failure must release the Redis coordination lock")
}

func TestProcessWikiIngestObsoleteDatabaseLeaseIsAcknowledgedWithoutQueueFailure(t *testing.T) {
	pending := &wikiQueuePendingRepoStub{leaseErr: &wikilease.FencedError{
		ExpectedTenantID: 7, ExpectedKnowledgeBaseID: "kb-1",
		ExpectedEpoch: 1, CurrentEpoch: 2,
	}}
	svc := &wikiIngestService{pendingRepo: pending}
	payload, err := json.Marshal(WikiIngestPayload{TenantID: 7, KnowledgeBaseID: "kb-1"})
	require.NoError(t, err)
	require.NoError(t, svc.ProcessWikiIngest(
		context.Background(), asynq.NewTask(types.TypeWikiIngest, payload),
	))
	require.Empty(t, pending.incrementedIDs)
	require.Empty(t, pending.archived)
	require.Empty(t, pending.deletedIDs)
	_, held := svc.liteLocks.Load("kb-1")
	require.False(t, held)
}

func TestWikiQueueSettlementContextPreservesDatabaseLeaseIdentity(t *testing.T) {
	identity := wikilease.Identity{
		TenantID: 7, KnowledgeBaseID: "kb-1", Epoch: 3,
		Token: "0123456789012345678901234567890123456789012",
	}
	parent, cancelParent := context.WithCancel(wikilease.WithIdentity(context.Background(), identity))
	cancelParent()
	settleCtx, cancelSettle := newWikiQueueSettlementContext(parent)
	defer cancelSettle()
	restored, ok := wikilease.IdentityFromContext(settleCtx)
	require.True(t, ok)
	require.Equal(t, identity, restored)
	require.NoError(t, settleCtx.Err(), "WithoutCancel must detach task cancellation for bounded settlement")
}

type wikiQueueKBServiceStub struct {
	interfaces.KnowledgeBaseService
	kb    *types.KnowledgeBase
	err   error
	calls int
}

func (s *wikiQueueKBServiceStub) GetKnowledgeBaseByIDOnly(context.Context, string) (*types.KnowledgeBase, error) {
	s.calls++
	if s.kb == nil || s.err != nil {
		return s.kb, s.err
	}
	copyKB := *s.kb
	if copyKB.TenantID == 0 {
		copyKB.TenantID = 42
	}
	// Legacy worker fixtures predate the dedicated control-plane field. Map
	// their explicit Wiki/summary model into DerivativeModelID in test code
	// only; production never performs this fallback at execution time.
	if copyKB.DerivativeModelID == "" {
		if copyKB.WikiConfig != nil && copyKB.WikiConfig.SynthesisModelID != "" {
			copyKB.DerivativeModelID = copyKB.WikiConfig.SynthesisModelID
		} else {
			copyKB.DerivativeModelID = copyKB.SummaryModelID
		}
	}
	return &copyKB, nil
}

func (s *wikiQueueKBServiceStub) GetKnowledgeBaseByID(ctx context.Context, id string) (*types.KnowledgeBase, error) {
	return s.GetKnowledgeBaseByIDOnly(ctx, id)
}

type wikiQueueKnowledgeServiceStub struct {
	interfaces.KnowledgeService
	mu        sync.RWMutex
	knowledge *types.Knowledge
	err       error
	calls     int
}

type wikiQueueModelServiceStub struct {
	interfaces.ModelService
	model chat.Chat
	err   error
}

type blockingWikiChatModel struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *blockingWikiChatModel) Chat(
	ctx context.Context,
	_ []chat.Message,
	_ *chat.ChatOptions,
) (*types.ChatResponse, error) {
	m.once.Do(func() { close(m.entered) })
	select {
	case <-m.release:
		return &types.ChatResponse{Content: "SUMMARY: updated\nupdated page body"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *blockingWikiChatModel) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, nil
}

func (m *blockingWikiChatModel) GetModelName() string { return "blocking" }
func (m *blockingWikiChatModel) GetModelID() string   { return "blocking" }

func (s *wikiQueueModelServiceStub) GetChatModel(context.Context, string) (chat.Chat, error) {
	return s.model, s.err
}

func (s *wikiQueueKnowledgeServiceStub) GetKnowledgeByIDOnly(context.Context, string) (*types.Knowledge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.knowledge == nil || s.err != nil {
		return s.knowledge, s.err
	}
	copyKnowledge := *s.knowledge
	if copyKnowledge.TenantID == 0 {
		copyKnowledge.TenantID = 42
	}
	if copyKnowledge.KnowledgeBaseID == "" {
		copyKnowledge.KnowledgeBaseID = "kb-1"
	}
	if copyKnowledge.ProcessingGeneration == "" {
		copyKnowledge.ProcessingGeneration = "test-generation"
	}
	if copyKnowledge.ParseStatus == "" {
		copyKnowledge.ParseStatus = types.ParseStatusCompleted
	}
	if copyKnowledge.ProcessedAt == nil &&
		(copyKnowledge.ParseStatus == types.ParseStatusProcessing ||
			copyKnowledge.ParseStatus == types.ParseStatusFinalizing ||
			copyKnowledge.ParseStatus == types.ParseStatusCompleted) {
		now := time.Unix(1_700_000_000, 0)
		copyKnowledge.ProcessedAt = &now
	}
	return &copyKnowledge, nil
}

func (s *wikiQueueKnowledgeServiceStub) mutateKnowledge(mutate func(*types.Knowledge)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.knowledge != nil && mutate != nil {
		mutate(s.knowledge)
	}
}

func TestMapOneDocumentTerminalGenerationDoesNotOpenWikiSpan(t *testing.T) {
	tracker, db := setupSpanTrackerTest(t)
	ctx := context.Background()
	_, attempt, err := tracker.OpenAttempt(ctx, "knowledge-terminal-wiki", "trace-terminal-wiki")
	require.NoError(t, err)
	postprocess := tracker.BeginStage(
		ctx,
		"knowledge-terminal-wiki",
		attempt,
		types.StagePostProcess,
		nil,
	)
	require.NotNil(t, postprocess)
	tracker.EndSpan(ctx, postprocess, nil)

	processedAt := time.Now().Add(-time.Minute)
	svc := &wikiIngestService{
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
			ID:                   "knowledge-terminal-wiki",
			TenantID:             42,
			KnowledgeBaseID:      "kb-1",
			ProcessingGeneration: "generation-1",
			ParseStatus:          types.ParseStatusCompleted,
			ProcessedAt:          &processedAt,
			WikiStatus:           types.WikiStatusCompleted,
		}},
		spanTracker: tracker,
	}
	result, updates, err := svc.mapOneDocument(
		ctx,
		nil,
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"},
		WikiPendingOp{
			Op:                   WikiOpIngest,
			KnowledgeID:          "knowledge-terminal-wiki",
			ProcessingGeneration: "generation-1",
			tenantID:             42,
		},
		&WikiBatchContext{},
	)
	require.NoError(t, err)
	require.Nil(t, result)
	require.Empty(t, updates)

	var wikiSpans int64
	require.NoError(t, db.Model(&types.KnowledgeProcessingSpan{}).
		Where("knowledge_id = ? AND name LIKE ?",
			"knowledge-terminal-wiki", "postprocess.wiki%").
		Count(&wikiSpans).Error)
	require.Zero(t, wikiSpans,
		"a delayed terminal duplicate is a no-op and must not create a fresh running span")
}

type wikiQueuePageServiceStub struct {
	interfaces.WikiPageService
	getPage         *types.WikiPage
	getErr          error
	indexPage       *types.WikiPage
	indexErr        error
	listPages       []*types.WikiPage
	listErr         error
	listSlugs       []string
	listSlugsErr    error
	listProvenance  []types.WikiPageSourceProvenance
	provenanceErr   error
	createErr       error
	updateErr       error
	deleteErr       error
	getCalls        int
	listCalls       int
	createCalls     int
	updated         []*types.WikiPage
	contentWrites   []*types.WikiPage
	metaClearIDs    [][]string
	contentClearIDs [][]string
	deleted         []string
}

func (s *wikiQueuePageServiceStub) GetPageBySlug(context.Context, string, string) (*types.WikiPage, error) {
	s.getCalls++
	return s.getPage, s.getErr
}

func (s *wikiQueuePageServiceStub) ListPagesBySourceRef(context.Context, string, string) ([]*types.WikiPage, error) {
	s.listCalls++
	return s.listPages, s.listErr
}

func (s *wikiQueuePageServiceStub) ListSlugsBySourceRef(context.Context, string, string) ([]string, error) {
	return s.listSlugs, s.listSlugsErr
}

func (s *wikiQueuePageServiceStub) ListSourceProvenanceBySourceRef(
	context.Context, string, string,
) ([]types.WikiPageSourceProvenance, error) {
	if s.provenanceErr != nil {
		return nil, s.provenanceErr
	}
	if s.listProvenance != nil {
		return slices.Clone(s.listProvenance), nil
	}
	if len(s.listPages) > 0 {
		rows := make([]types.WikiPageSourceProvenance, 0, len(s.listPages))
		for _, page := range s.listPages {
			if page != nil {
				rows = append(rows, types.WikiPageSourceProvenance{
					Slug: page.Slug, PageType: page.PageType,
					ChunkRefs: slices.Clone(page.ChunkRefs),
				})
			}
		}
		return rows, nil
	}
	rows := make([]types.WikiPageSourceProvenance, 0, len(s.listSlugs))
	for _, slug := range s.listSlugs {
		rows = append(rows, types.WikiPageSourceProvenance{Slug: slug})
	}
	return rows, nil
}

func (s *wikiQueuePageServiceStub) CreatePage(_ context.Context, page *types.WikiPage) (*types.WikiPage, error) {
	s.createCalls++
	if s.createErr != nil {
		return nil, s.createErr
	}
	return page, nil
}

func (s *wikiQueuePageServiceStub) GetIndex(context.Context, string) (*types.WikiPage, error) {
	return s.indexPage, s.indexErr
}

func (s *wikiQueuePageServiceStub) UpdatePage(ctx context.Context, page *types.WikiPage) (*types.WikiPage, error) {
	copyPage := *page
	copyPage.SourceRefs = slices.Clone(page.SourceRefs)
	copyPage.ChunkRefs = slices.Clone(page.ChunkRefs)
	s.contentWrites = append(s.contentWrites, &copyPage)
	s.contentClearIDs = append(s.contentClearIDs, wikidelete.ClearSources(ctx))
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	return page, nil
}

func (s *wikiQueuePageServiceStub) UpdatePageMeta(ctx context.Context, page *types.WikiPage) error {
	copyPage := *page
	copyPage.SourceRefs = slices.Clone(page.SourceRefs)
	s.updated = append(s.updated, &copyPage)
	s.metaClearIDs = append(s.metaClearIDs, wikidelete.ClearSources(ctx))
	return s.updateErr
}

func (s *wikiQueuePageServiceStub) DeletePage(_ context.Context, _ string, slug string) error {
	s.deleted = append(s.deleted, slug)
	return s.deleteErr
}

type wikiQueueChunkRepoStub struct {
	interfaces.ChunkRepository
	ids []string
	err error
}

func (s *wikiQueueChunkRepoStub) ListChunkIDsByKnowledgeIDUnscoped(
	context.Context,
	uint64,
	string,
) ([]string, error) {
	return slices.Clone(s.ids), s.err
}

type wikiQueueLogEntryServiceStub struct {
	interfaces.WikiLogEntryService
	err     error
	entries []*types.WikiLogEntry
}

func (s *wikiQueueLogEntryServiceStub) AppendBatch(_ context.Context, entries []*types.WikiLogEntry) error {
	for _, entry := range entries {
		copyEntry := *entry
		s.entries = append(s.entries, &copyEntry)
	}
	return s.err
}

type wikiQueueTaskEnqueuerStub struct {
	err      error
	tasks    []*asynq.Task
	opts     [][]asynq.Option
	prepared map[string]*asynq.Task
	resumed  map[string]bool
	aborts   int
	binds    int
}

func (e *wikiQueueTaskEnqueuerStub) Enqueue(
	task *asynq.Task,
	opts ...asynq.Option,
) (*asynq.TaskInfo, error) {
	e.tasks = append(e.tasks, task)
	e.opts = append(e.opts, slices.Clone(opts))
	if e.err != nil {
		return nil, e.err
	}
	return &asynq.TaskInfo{ID: "test-task", Type: task.Type(), Queue: "low"}, nil
}

func TestEnqueueWikiIngestSchedulesDocumentMapAndKBCommitForDistributedRepository(t *testing.T) {
	base := &wikiQueuePendingRepoStub{}
	pending := &wikiDistributedPendingRepoStub{wikiQueuePendingRepoStub: base}
	enqueuer := &wikiQueueTaskEnqueuerStub{}

	result, err := EnqueueWikiIngest(
		context.Background(),
		enqueuer,
		pending,
		42,
		"kb-1",
		"knowledge-1",
		"generation-1",
	)
	require.NoError(t, err)
	require.True(t, result.PendingPersisted)
	require.True(t, result.MapScheduled)
	require.True(t, result.TriggerScheduled)
	require.Len(t, enqueuer.tasks, 2)

	var mapPayload WikiIngestPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &mapPayload))
	assert.Equal(t, wikiTaskModeMap, mapPayload.TaskMode)
	assert.Equal(t, "knowledge-1:generation-1", mapPayload.MapDedupKey)
	var commitPayload WikiIngestPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[1].Payload(), &commitPayload))
	assert.Empty(t, commitPayload.TaskMode)
	assert.Equal(t, "kb-1", commitPayload.KnowledgeBaseID)
}

func TestProcessWikiMapPublishesRecoveredCheckpointAndWakesCommit(t *testing.T) {
	op := WikiPendingOp{
		Op: WikiOpIngest, KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-1",
		Prepared: &wikiPreparedIngest{
			DocTitle: "Document", Summary: "summary", Updates: []SlugUpdate{},
		},
		MapFinished: true,
	}
	row := wikiPendingRow(91, op)
	row.TenantID = 42
	row.ScopeID = "kb-1"
	row.DedupKey = "knowledge-1:generation-1"
	base := &wikiQueuePendingRepoStub{rows: []*types.TaskPendingOp{row}}
	pending := &wikiDistributedPendingRepoStub{wikiQueuePendingRepoStub: base}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{pendingRepo: pending, task: enqueuer}
	payload := WikiIngestPayload{
		TenantID: 42, KnowledgeBaseID: "kb-1", TaskMode: wikiTaskModeMap,
		MapDedupKey: "knowledge-1:generation-1",
		KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-1",
	}
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)

	require.NoError(t, svc.Handle(
		context.Background(), asynq.NewTask(types.TypeWikiIngest, encoded),
	))
	require.NotNil(t, row.MapReadyAt)
	require.Len(t, enqueuer.tasks, 1)
	var commit WikiIngestPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &commit))
	assert.Empty(t, commit.TaskMode)
	assert.Equal(t, uint8(1), commit.WakePhase)
	assert.Equal(t, 1, base.checkpointCalls)
}

func TestProcessWikiMapDuplicateDeliveryUsesDocumentLeaseAndCoalescedSuccessor(t *testing.T) {
	row := wikiPendingRow(92, WikiPendingOp{
		Op: WikiOpIngest, KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-1",
	})
	row.TenantID = 42
	row.ScopeID = "kb-1"
	row.DedupKey = "knowledge-1:generation-1"
	base := &wikiQueuePendingRepoStub{rows: []*types.TaskPendingOp{row}}
	pending := &wikiDistributedPendingRepoStub{wikiQueuePendingRepoStub: base}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{pendingRepo: pending, task: enqueuer}
	payload := WikiIngestPayload{
		TenantID: 42, KnowledgeBaseID: "kb-1", TaskMode: wikiTaskModeMap,
		MapDedupKey: "knowledge-1:generation-1",
	}
	svc.liteLocks.Store("map:"+wikiMapActiveKey(payload), struct{}{})
	defer svc.liteLocks.Delete("map:" + wikiMapActiveKey(payload))
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)

	require.NoError(t, svc.ProcessWikiMap(
		context.Background(), asynq.NewTask(types.TypeWikiIngest, encoded),
	))
	require.Nil(t, row.MapReadyAt)
	require.Len(t, enqueuer.tasks, 1)
	var successor WikiIngestPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &successor))
	assert.Equal(t, wikiTaskModeMap, successor.TaskMode)
	assert.Equal(t, uint8(1), successor.WakePhase)
	assert.Zero(t, base.checkpointCalls)
}

func TestDistributedWikiMapProviderRetryIsSpreadAfterCircuitBoundary(t *testing.T) {
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	pending := &wikiQueuePendingRepoStub{}
	svc := &wikiIngestService{pendingRepo: pending, task: enqueuer}
	payload := WikiIngestPayload{
		TenantID:        42,
		KnowledgeBaseID: "kb-1",
		MapDedupKey:     "knowledge-1:generation-1",
	}
	base := 5 * time.Minute
	providerErr := &modeladmission.CircuitOpenError{
		Kind:       modeladmission.KindChat,
		RetryAfter: base,
	}

	require.NoError(t, svc.recordDistributedMapFailure(
		context.Background(),
		payload,
		WikiPendingOp{KnowledgeID: "knowledge-1", dbID: 91},
		providerErr,
	))
	require.Equal(t, []int64{91}, pending.touchedIDs)
	require.Empty(t, pending.incrementedIDs)
	require.Len(t, enqueuer.tasks, 1)
	delay, ok := optionDuration(enqueuer.opts[0], asynq.ProcessInOpt)
	require.True(t, ok)
	require.GreaterOrEqual(t, delay, base)
	require.Less(t, delay, base+modeladmission.ProviderRetrySpreadWindow(base))
	require.Equal(t, delay, modeladmission.SpreadProviderRetry(
		base,
		"wiki-map\x0042\x00kb-1\x00knowledge-1:generation-1",
	))
}

func TestDistributedWikiMapRealProviderFailureConsumesAttemptBudget(t *testing.T) {
	row := wikiPendingRow(92, WikiPendingOp{
		Op: WikiOpIngest, KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-1",
	})
	row.TenantID = 42
	row.ScopeID = "kb-1"
	row.DedupKey = "knowledge-1:generation-1"
	base := &wikiQueuePendingRepoStub{
		rows:          []*types.TaskPendingOp{row},
		incrCount:     1,
		countFromRows: true,
	}
	pending := &wikiDistributedPendingRepoStub{wikiQueuePendingRepoStub: base}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{pendingRepo: pending, task: enqueuer}
	payload := WikiIngestPayload{
		TenantID: 42, KnowledgeBaseID: "kb-1", TaskMode: wikiTaskModeMap,
		MapDedupKey: "knowledge-1:generation-1",
	}
	providerErr := workretry.ConsumeProviderFailure(
		&modeladmission.ProviderUnavailableError{
			Kind:       modeladmission.KindChat,
			RetryAfter: time.Minute,
			Cause:      context.DeadlineExceeded,
		},
	)

	require.NoError(t, svc.recordDistributedMapFailure(
		context.Background(),
		payload,
		WikiPendingOp{
			Op: WikiOpIngest, KnowledgeID: "knowledge-1",
			ProcessingGeneration: "generation-1", dbID: row.ID,
		},
		providerErr,
	))
	require.Equal(t, []int64{row.ID}, base.incrementedIDs)
	require.Empty(t, base.touchedIDs)
	require.Len(t, enqueuer.tasks, 1)
	delay, ok := optionDuration(enqueuer.opts[0], asynq.ProcessInOpt)
	require.True(t, ok)
	require.Equal(t, wikiFollowUpDelay, delay)
}

func (e *wikiQueueTaskEnqueuerStub) PrepareDocumentWorkflow(
	_ context.Context,
	task *asynq.Task,
	_ ...asynq.Option,
) (*documentqueue.Workflow, bool, error) {
	if e.err != nil {
		return nil, false, e.err
	}
	var identity struct {
		TenantID             uint64 `json:"tenant_id"`
		KnowledgeID          string `json:"knowledge_id"`
		KnowledgeBaseID      string `json:"knowledge_base_id"`
		ProcessingGeneration string `json:"processing_generation"`
	}
	if err := json.Unmarshal(task.Payload(), &identity); err != nil {
		return nil, false, err
	}
	id := "workflow-" + identity.KnowledgeID + "-" + identity.ProcessingGeneration
	if e.prepared == nil {
		e.prepared = make(map[string]*asynq.Task)
	}
	created := e.prepared[id] == nil
	e.prepared[id] = task
	return &documentqueue.Workflow{
		ID: id, TenantID: identity.TenantID,
		KnowledgeID: identity.KnowledgeID, KnowledgeBaseID: identity.KnowledgeBaseID,
		ProcessingGeneration: identity.ProcessingGeneration,
		TaskType:             task.Type(), Payload: append([]byte(nil), task.Payload()...),
		State: documentqueue.StatePreparing,
	}, created, nil
}

func (e *wikiQueueTaskEnqueuerStub) AbortDocumentWorkflow(
	_ context.Context,
	binding documentqueue.WorkflowBinding,
	_ string,
) error {
	e.aborts++
	delete(e.prepared, binding.WorkflowID)
	return nil
}

func (e *wikiQueueTaskEnqueuerStub) BindDocumentWorkflowTransitionTx(
	tx *gorm.DB,
	binding documentqueue.WorkflowBinding,
	transition func(*gorm.DB) error,
) error {
	e.binds++
	if tx == nil || transition == nil || e.prepared[binding.WorkflowID] == nil {
		return errors.New("prepared workflow binding is unavailable")
	}
	if err := transition(tx); err != nil {
		return err
	}
	var bound int64
	if err := tx.Table("knowledges").Where(
		"id = ? AND tenant_id = ? AND knowledge_base_id = ? AND parse_status = ? AND processing_generation = ? AND processing_owner = ? AND processing_workflow_id = ?",
		binding.KnowledgeID, binding.TenantID, binding.KnowledgeBaseID,
		types.ParseStatusPending, binding.ProcessingGeneration, binding.ProcessingOwner,
		binding.WorkflowID,
	).Count(&bound).Error; err != nil {
		return err
	}
	if bound != 1 {
		return errors.New("prepared workflow binding validation failed")
	}
	return nil
}

func (e *wikiQueueTaskEnqueuerStub) ResumeDocumentWorkflow(
	_ context.Context,
	binding documentqueue.WorkflowBinding,
) (*asynq.TaskInfo, error) {
	if e.err != nil {
		return nil, e.err
	}
	task := e.prepared[binding.WorkflowID]
	if task == nil {
		return nil, errors.New("prepared workflow not found")
	}
	if e.resumed == nil {
		e.resumed = make(map[string]bool)
	}
	if !e.resumed[binding.WorkflowID] {
		e.tasks = append(e.tasks, task)
		e.resumed[binding.WorkflowID] = true
	}
	return &asynq.TaskInfo{ID: binding.WorkflowID, Type: task.Type(), Queue: types.QueueDocument}, nil
}

type wikiQueueDeadLetterRepoStub struct {
	interfaces.TaskDeadLetterRepository
	insertErr error
	inserted  []*types.TaskDeadLetter
	ctxErr    error
}

type countingWikiChatModel struct {
	mu       sync.Mutex
	calls    int
	response string
}

func (m *countingWikiChatModel) Chat(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (*types.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return &types.ChatResponse{Content: m.response}, nil
}

func (m *countingWikiChatModel) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, errors.New("streaming is not used by wiki replay tests")
}

func (m *countingWikiChatModel) GetModelName() string { return "counting" }
func (m *countingWikiChatModel) GetModelID() string   { return "counting" }

func (m *countingWikiChatModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// wikiReplayPageServiceStub models the durable boundaries relevant to an
// ingest replay: ordinary page writes checkpoint applied_page_slugs, index
// writes persist their source_op_id marker on the index row, and publication
// is an independent metadata transition. Reads return copies so a failed
// write cannot accidentally mutate the committed test state through aliases.
type wikiReplayPageServiceStub struct {
	interfaces.WikiPageService
	mu sync.Mutex

	pending *wikiQueuePendingRepoStub
	pages   map[string]*types.WikiPage
	index   *types.WikiPage

	indexWriteFailures       int
	publicationWriteFailures int
	contentWritesBySlug      map[string]int
	publicationWritesBySlug  map[string]int
}

func cloneReplayWikiPage(page *types.WikiPage) *types.WikiPage {
	if page == nil {
		return nil
	}
	copyPage := *page
	copyPage.SourceRefs = slices.Clone(page.SourceRefs)
	copyPage.ChunkRefs = slices.Clone(page.ChunkRefs)
	copyPage.InLinks = slices.Clone(page.InLinks)
	copyPage.OutLinks = slices.Clone(page.OutLinks)
	copyPage.Aliases = slices.Clone(page.Aliases)
	copyPage.CategoryPath = slices.Clone(page.CategoryPath)
	copyPage.PageMetadata = slices.Clone(page.PageMetadata)
	return &copyPage
}

func (s *wikiReplayPageServiceStub) GetPageBySlug(
	_ context.Context,
	_ string,
	slug string,
) (*types.WikiPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	page := s.pages[slug]
	if page == nil {
		return nil, apprepo.ErrWikiPageNotFound
	}
	return cloneReplayWikiPage(page), nil
}

func (s *wikiReplayPageServiceStub) GetIndex(context.Context, string) (*types.WikiPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneReplayWikiPage(s.index), nil
}

func (s *wikiReplayPageServiceStub) CreatePage(ctx context.Context, page *types.WikiPage) (*types.WikiPage, error) {
	return s.commitContentPage(ctx, page)
}

func (s *wikiReplayPageServiceStub) UpdatePage(ctx context.Context, page *types.WikiPage) (*types.WikiPage, error) {
	return s.commitContentPage(ctx, page)
}

func (s *wikiReplayPageServiceStub) commitContentPage(
	ctx context.Context,
	page *types.WikiPage,
) (*types.WikiPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if page.Slug == "index" {
		if s.indexWriteFailures > 0 {
			s.indexWriteFailures--
			return nil, errors.New("injected index write failure")
		}
		s.index = cloneReplayWikiPage(page)
		s.contentWritesBySlug[page.Slug]++
		return cloneReplayWikiPage(page), nil
	}
	s.pages[page.Slug] = cloneReplayWikiPage(page)
	s.contentWritesBySlug[page.Slug]++
	s.markPageAppliedLocked(ctx, page.Slug)
	return cloneReplayWikiPage(page), nil
}

func (s *wikiReplayPageServiceStub) UpdatePageMeta(_ context.Context, page *types.WikiPage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicationWriteFailures > 0 {
		s.publicationWriteFailures--
		return errors.New("injected publication write failure")
	}
	s.pages[page.Slug] = cloneReplayWikiPage(page)
	s.publicationWritesBySlug[page.Slug]++
	return nil
}

func (s *wikiReplayPageServiceStub) UpdateAutoLinkedContent(_ context.Context, page *types.WikiPage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages[page.Slug] = cloneReplayWikiPage(page)
	return nil
}

func (s *wikiReplayPageServiceStub) ListBySlugs(
	_ context.Context,
	_ string,
	slugs []string,
) (map[string]*types.WikiPageLite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]*types.WikiPageLite)
	for _, slug := range slugs {
		if page := s.pages[slug]; page != nil {
			result[slug] = &types.WikiPageLite{
				Slug: slug, Title: page.Title, PageType: page.PageType,
				Status: page.Status, Aliases: slices.Clone(page.Aliases), OutLinks: slices.Clone(page.OutLinks),
			}
		}
	}
	return result, nil
}

func (s *wikiReplayPageServiceStub) ListSummariesByKnowledgeIDs(
	context.Context,
	string,
	[]string,
) (map[string]string, error) {
	return map[string]string{}, nil
}

func (s *wikiReplayPageServiceStub) ExistsSlugs(
	_ context.Context,
	_ string,
	slugs []string,
) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]bool, len(slugs))
	for _, slug := range slugs {
		page := s.pages[slug]
		result[slug] = page != nil && page.Status != types.WikiPageStatusArchived
	}
	return result, nil
}

func (s *wikiReplayPageServiceStub) markPageAppliedLocked(ctx context.Context, slug string) {
	identities := wikiingestguard.Identities(ctx)
	if len(identities) == 0 || s.pending == nil {
		return
	}
	for _, row := range s.pending.rows {
		if row == nil {
			continue
		}
		var op WikiPendingOp
		if err := json.Unmarshal(row.Payload, &op); err != nil {
			continue
		}
		matched := false
		for _, identity := range identities {
			if identity.KnowledgeID == op.KnowledgeID &&
				identity.ProcessingGeneration == op.ProcessingGeneration {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if !slices.Contains(op.AppliedPageSlugs, slug) {
			op.AppliedPageSlugs = append(op.AppliedPageSlugs, slug)
			slices.Sort(op.AppliedPageSlugs)
		}
		encoded, err := json.Marshal(op)
		if err == nil {
			row.Payload = encoded
		}
	}
}

type wikiReplayLogEntryServiceStub struct {
	interfaces.WikiLogEntryService
	mu                  sync.Mutex
	failuresAfterCommit int
	appendCalls         int
	bySourceOpID        map[int64]*types.WikiLogEntry
}

func (s *wikiReplayLogEntryServiceStub) AppendBatch(_ context.Context, entries []*types.WikiLogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendCalls++
	for _, entry := range entries {
		if entry == nil || entry.SourceOpID == nil {
			continue
		}
		if _, exists := s.bySourceOpID[*entry.SourceOpID]; !exists {
			copyEntry := *entry
			s.bySourceOpID[*entry.SourceOpID] = &copyEntry
		}
	}
	if s.failuresAfterCommit > 0 {
		s.failuresAfterCommit--
		return errors.New("injected log response failure after commit")
	}
	return nil
}

func (r *wikiQueueDeadLetterRepoStub) Insert(ctx context.Context, row *types.TaskDeadLetter) error {
	r.ctxErr = ctx.Err()
	if r.insertErr != nil {
		return r.insertErr
	}
	copyRow := *row
	copyRow.Payload = slices.Clone(row.Payload)
	r.inserted = append(r.inserted, &copyRow)
	return nil
}

func hasAsynqOption(opts []asynq.Option, optionType asynq.OptionType) bool {
	for _, opt := range opts {
		if opt.Type() == optionType {
			return true
		}
	}
	return false
}

func optionDuration(opts []asynq.Option, optionType asynq.OptionType) (time.Duration, bool) {
	for _, opt := range opts {
		if opt.Type() != optionType {
			continue
		}
		d, ok := opt.Value().(time.Duration)
		return d, ok
	}
	return 0, false
}

func wikiPendingRows(count int) []*types.TaskPendingOp {
	rows := make([]*types.TaskPendingOp, 0, count)
	for i := 1; i <= count; i++ {
		rows = append(rows, wikiPendingRow(int64(i), WikiPendingOp{
			Op: WikiOpIngest, KnowledgeID: fmt.Sprintf("knowledge-%d", i),
		}))
	}
	return rows
}

func wikiPendingRow(id int64, op WikiPendingOp) *types.TaskPendingOp {
	if op.Op == WikiOpIngest && op.ProcessingGeneration == "" {
		op.ProcessingGeneration = "test-generation"
	}
	payload, err := json.Marshal(op)
	if err != nil {
		panic(err)
	}
	dedupKey := op.KnowledgeID
	if op.Op == WikiOpIngest {
		dedupKey, err = wikiqueue.IngestDedupKey(op.KnowledgeID, op.ProcessingGeneration)
		if err != nil {
			panic(err)
		}
	}
	return &types.TaskPendingOp{
		ID: id, TenantID: 42, TaskType: wikiTaskType, Scope: wikiTaskScope, ScopeID: "kb-1",
		Op: op.Op, DedupKey: dedupKey, Payload: payload,
	}
}

func TestEnqueueWikiIngestUniqueDuplicateIsSuccess(t *testing.T) {
	repo := &wikiQueuePendingRepoStub{}
	enqueuer := &wikiQueueTaskEnqueuerStub{err: asynq.ErrDuplicateTask}
	ctx := context.WithValue(context.Background(), types.LanguageContextKey, "en-US")

	result, err := EnqueueWikiIngest(ctx, enqueuer, repo, 42, "kb-1", "knowledge-1", "generation-1")
	if err != nil {
		t.Fatalf("EnqueueWikiIngest() error = %v, want nil duplicate-trigger result", err)
	}
	if !result.PendingPersisted || !result.TriggerScheduled {
		t.Fatalf("EnqueueWikiIngest() result = %+v, want durable row and existing/scheduled trigger", result)
	}
	if len(repo.enqueued) != 1 {
		t.Fatalf("persisted pending rows = %d, want 1", len(repo.enqueued))
	}
	var op WikiPendingOp
	if err := json.Unmarshal(repo.enqueued[0].Payload, &op); err != nil {
		t.Fatalf("unmarshal pending payload: %v", err)
	}
	if op.Language != "en-US" {
		t.Fatalf("pending op language = %q, want en-US", op.Language)
	}
	if op.ProcessingGeneration != "generation-1" {
		t.Fatalf("pending op processing generation = %q, want generation-1", op.ProcessingGeneration)
	}
	if repo.enqueued[0].DedupKey != "knowledge-1:generation-1" {
		t.Fatalf("pending op dedup key = %q, want generation-scoped key", repo.enqueued[0].DedupKey)
	}
	if len(enqueuer.tasks) != 1 {
		t.Fatalf("trigger attempts = %d, want 1", len(enqueuer.tasks))
	}
	if !hasAsynqOption(enqueuer.opts[0], asynq.UniqueOpt) {
		t.Fatal("initial trigger is missing asynq.Unique")
	}
	if ttl, ok := optionDuration(enqueuer.opts[0], asynq.UniqueOpt); !ok || ttl != wikiTriggerUniqueTTL {
		t.Fatalf("Unique TTL = %v (ok=%v), want %v", ttl, ok, wikiTriggerUniqueTTL)
	}
	if timeout, ok := optionDuration(enqueuer.opts[0], asynq.TimeoutOpt); !ok || timeout != wikiIngestTaskTimeout {
		t.Fatalf("task timeout = %v (ok=%v), want %v", timeout, ok, wikiIngestTaskTimeout)
	}

	var trigger WikiIngestPayload
	if err := json.Unmarshal(enqueuer.tasks[0].Payload(), &trigger); err != nil {
		t.Fatalf("unmarshal trigger payload: %v", err)
	}
	if trigger.KnowledgeBaseID != "kb-1" || trigger.TenantID != 42 {
		t.Fatalf("trigger identity = tenant:%d kb:%q", trigger.TenantID, trigger.KnowledgeBaseID)
	}
	if trigger.Language != "" || trigger.TracingContext != (types.TracingContext{}) {
		t.Fatalf("trigger payload must remain stable, got language=%q tracing=%+v", trigger.Language, trigger.TracingContext)
	}
}

func TestEnqueueWikiIngestRejectsMissingGenerationBeforePersistence(t *testing.T) {
	repo := &wikiQueuePendingRepoStub{}
	enqueuer := &wikiQueueTaskEnqueuerStub{}

	result, err := EnqueueWikiIngest(
		context.Background(), enqueuer, repo, 42, "kb-1", "knowledge-1", "",
	)
	require.Error(t, err)
	require.False(t, result.PendingPersisted)
	require.Empty(t, repo.enqueued)
	require.Empty(t, enqueuer.tasks)
}

func TestEnqueueWikiIngestSettledGenerationDoesNotRecreateRowOrTrigger(t *testing.T) {
	identity := wikiIngestIdentity(42, "kb-1", "knowledge-1", "generation-1")
	repo := &wikiQueuePendingRepoStub{
		enqueueErr: wikiingestguard.NewStaleIdentityError(identity),
	}
	enqueuer := &wikiQueueTaskEnqueuerStub{}

	result, err := EnqueueWikiIngest(
		context.Background(), enqueuer, repo,
		42, "kb-1", "knowledge-1", "generation-1",
	)
	require.NoError(t, err)
	require.True(t, result.PendingPersisted,
		"an already-settled generation satisfies the durable intent")
	require.True(t, result.AlreadySettled)
	require.False(t, result.TriggerScheduled)
	require.Empty(t, repo.enqueued)
	require.Empty(t, enqueuer.tasks)
}

func TestEnqueueWikiIngestTriggerFailureLeavesDurableRowAndReturnsError(t *testing.T) {
	triggerErr := errors.New("redis unavailable")
	repo := &wikiQueuePendingRepoStub{}
	enqueuer := &wikiQueueTaskEnqueuerStub{err: triggerErr}

	result, err := EnqueueWikiIngest(context.Background(), enqueuer, repo, 42, "kb-1", "knowledge-1", "generation-1")
	if !errors.Is(err, triggerErr) {
		t.Fatalf("EnqueueWikiIngest() error = %v, want wrapped trigger error", err)
	}
	if !result.PendingPersisted || result.TriggerScheduled {
		t.Fatalf("EnqueueWikiIngest() result = %+v, want persisted row and failed trigger", result)
	}
	if len(repo.enqueued) != 1 {
		t.Fatalf("persisted pending rows = %d, want 1", len(repo.enqueued))
	}
	if len(repo.deletedIDs) != 0 {
		t.Fatalf("trigger failure must not delete durable row, deletes=%v", repo.deletedIDs)
	}
}

func TestEnqueueWikiIngestPersistFailureIsNotHidden(t *testing.T) {
	persistErr := errors.New("postgres unavailable")
	repo := &wikiQueuePendingRepoStub{enqueueErr: persistErr}
	enqueuer := &wikiQueueTaskEnqueuerStub{}

	result, err := EnqueueWikiIngest(context.Background(), enqueuer, repo, 42, "kb-1", "knowledge-1", "generation-1")
	if !errors.Is(err, persistErr) {
		t.Fatalf("EnqueueWikiIngest() error = %v, want wrapped persist error", err)
	}
	if result.PendingPersisted || !result.TriggerScheduled {
		t.Fatalf("EnqueueWikiIngest() result = %+v, want failed persistence and successful wake-up", result)
	}
	if len(enqueuer.tasks) != 1 {
		t.Fatalf("worker should still wake an existing queue, trigger attempts=%d", len(enqueuer.tasks))
	}
}

func TestEnqueueWikiRetractTriggerFailureLeavesDurableRow(t *testing.T) {
	triggerErr := errors.New("redis unavailable")
	repo := &wikiQueuePendingRepoStub{}
	enqueuer := &wikiQueueTaskEnqueuerStub{err: triggerErr}

	result, err := EnqueueWikiRetract(context.Background(), enqueuer, repo, WikiRetractPayload{
		TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
	})
	if !errors.Is(err, triggerErr) {
		t.Fatalf("EnqueueWikiRetract() error = %v, want wrapped trigger error", err)
	}
	if !result.PendingPersisted || result.TriggerScheduled {
		t.Fatalf("EnqueueWikiRetract() result = %+v, want persisted row and failed trigger", result)
	}
	if len(repo.enqueued) != 1 || repo.enqueued[0].Op != WikiOpRetract {
		t.Fatalf("persisted retract rows = %+v, want one retract", repo.enqueued)
	}
}

func TestEnqueueWikiRetractPersistFailureIsNotHidden(t *testing.T) {
	persistErr := errors.New("postgres unavailable")
	repo := &wikiQueuePendingRepoStub{enqueueErr: persistErr}
	enqueuer := &wikiQueueTaskEnqueuerStub{}

	result, err := EnqueueWikiRetract(context.Background(), enqueuer, repo, WikiRetractPayload{
		TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
	})
	if !errors.Is(err, persistErr) {
		t.Fatalf("EnqueueWikiRetract() error = %v, want wrapped persistence error", err)
	}
	if result.PendingPersisted || !result.TriggerScheduled {
		t.Fatalf("EnqueueWikiRetract() result = %+v, want failed persistence and successful wake-up", result)
	}
}

func TestSettleWikiQueueCancelledBeforeSettlementDoesNotMutateQueue(t *testing.T) {
	repo := &wikiQueuePendingRepoStub{pendingCount: 3, incrCount: 1}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{pendingRepo: repo, task: enqueuer}
	parentCtx, cancel := context.WithCancel(context.Background())
	cancel()

	followUp, err := svc.settleWikiQueue(
		parentCtx,
		context.Background(),
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"},
		[]int64{10, 11},
		[]WikiPendingOp{{Op: WikiOpIngest, KnowledgeID: "failed-doc", dbID: 12}},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("settleWikiQueue() error = %v, want context.Canceled", err)
	}
	if followUp {
		t.Fatal("cancelled business batch must not schedule follow-up settlement")
	}
	if len(repo.deletedIDs) != 0 || len(repo.incrementedIDs) != 0 || len(enqueuer.tasks) != 0 {
		t.Fatalf("cancelled business batch mutated queue: deletes=%v increments=%v triggers=%d",
			repo.deletedIDs, repo.incrementedIDs, len(enqueuer.tasks))
	}
}

func TestWikiQueueSettlementContextSurvivesCancellationAfterEntry(t *testing.T) {
	parentCtx, cancelParent := context.WithCancel(context.Background())
	settleCtx, cancelSettle := newWikiQueueSettlementContext(parentCtx)
	defer cancelSettle()

	cancelParent()
	if err := parentCtx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent context error = %v, want context.Canceled", err)
	}
	if err := settleCtx.Err(); err != nil {
		t.Fatalf("settlement context inherited parent cancellation: %v", err)
	}
}

func TestSettleWikiQueueLostLeaseDoesNotMutateQueue(t *testing.T) {
	repo := &wikiQueuePendingRepoStub{pendingCount: 3, incrCount: 1}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{pendingRepo: repo, task: enqueuer}
	leaseErr := errors.New("lease ownership lost")
	leaseCtx, cancelLease := context.WithCancelCause(context.Background())
	cancelLease(leaseErr)

	followUp, err := svc.settleWikiQueue(
		context.Background(),
		leaseCtx,
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"},
		[]int64{10, 11},
		[]WikiPendingOp{{Op: WikiOpIngest, KnowledgeID: "failed-doc", dbID: 12}},
	)
	if !errors.Is(err, leaseErr) {
		t.Fatalf("settleWikiQueue() error = %v, want lease error", err)
	}
	if followUp {
		t.Fatal("lost lease must not schedule follow-up settlement")
	}
	if len(repo.deletedIDs) != 0 || len(repo.incrementedIDs) != 0 || len(enqueuer.tasks) != 0 {
		t.Fatalf("lost lease mutated queue: deletes=%v increments=%v triggers=%d",
			repo.deletedIDs, repo.incrementedIDs, len(enqueuer.tasks))
	}
}

func TestSettleWikiQueueActiveBatchUsesDetachedContextAndAlternatingUniqueFollowUp(t *testing.T) {
	repo := &wikiQueuePendingRepoStub{pendingCount: 3, incrCount: 1}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{pendingRepo: repo, task: enqueuer}

	followUp, err := svc.settleWikiQueue(
		context.Background(),
		context.Background(),
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"},
		[]int64{10, 11},
		[]WikiPendingOp{{Op: WikiOpIngest, KnowledgeID: "failed-doc", dbID: 12}},
	)
	if err != nil {
		t.Fatalf("settleWikiQueue() error = %v", err)
	}
	if !followUp {
		t.Fatal("settleWikiQueue() followUp = false, want true")
	}
	if repo.deleteCtxErr != nil || repo.incrCtxErr != nil || repo.countCtxErr != nil {
		t.Fatalf("settlement context was not live: delete=%v incr=%v count=%v",
			repo.deleteCtxErr, repo.incrCtxErr, repo.countCtxErr)
	}
	if len(repo.deletedIDs) != 1 || !slices.Equal(repo.deletedIDs[0], []int64{10, 11}) {
		t.Fatalf("deleted ids = %v, want [[10 11]]", repo.deletedIDs)
	}
	if !slices.Equal(repo.incrementedIDs, []int64{12}) {
		t.Fatalf("incremented ids = %v, want [12]", repo.incrementedIDs)
	}
	if len(enqueuer.tasks) != 1 {
		t.Fatalf("follow-up attempts = %d, want 1", len(enqueuer.tasks))
	}
	if !hasAsynqOption(enqueuer.opts[0], asynq.UniqueOpt) {
		t.Fatal("alternating follow-up must carry Unique")
	}
	if delay, ok := optionDuration(enqueuer.opts[0], asynq.ProcessInOpt); !ok || delay != wikiFollowUpDelay {
		t.Fatalf("follow-up delay = %v (ok=%v), want %v", delay, ok, wikiFollowUpDelay)
	}
	var followUpPayload WikiIngestPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &followUpPayload))
	require.Equal(t, uint8(1), followUpPayload.WakePhase)
}

func TestSettleWikiQueueSeparatesRealFailuresFromProviderDeferrals(t *testing.T) {
	repo := &wikiQueuePendingRepoStub{pendingCount: 2, incrCount: 1}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{pendingRepo: repo, task: enqueuer}
	circuitErr := &modeladmission.CircuitOpenError{
		Kind:       modeladmission.KindChat,
		RetryAfter: 3 * time.Minute,
	}

	followUp, err := svc.settleWikiQueueWithDeferrals(
		context.Background(),
		context.Background(),
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"},
		[]int64{10},
		[]WikiPendingOp{{Op: WikiOpIngest, KnowledgeID: "timed-out", dbID: 11}},
		[]WikiPendingOp{{Op: WikiOpIngest, KnowledgeID: "circuit-rejected", dbID: 12}},
		circuitErr,
	)
	require.NoError(t, err)
	require.True(t, followUp)
	require.Equal(t, [][]int64{{10}}, repo.deletedIDs)
	require.Equal(t, []int64{11}, repo.incrementedIDs)
	require.Equal(t, []int64{12}, repo.touchedIDs)
	require.Len(t, enqueuer.tasks, 1)
	delay, ok := optionDuration(enqueuer.opts[0], asynq.ProcessInOpt)
	require.True(t, ok)
	require.GreaterOrEqual(t, delay, circuitErr.RetryAfter)
	require.Less(t, delay, circuitErr.RetryAfter+modeladmission.ProviderRetrySpreadWindow(circuitErr.RetryAfter))
}

func TestScheduleLockConflictFollowUpUsesAlternatingUniqueTask(t *testing.T) {
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{task: enqueuer}
	payload := WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-locked"}

	scheduled, err := svc.scheduleLockConflictFollowUp(context.Background(), payload)
	if err != nil {
		t.Fatalf("scheduleLockConflictFollowUp() error = %v", err)
	}
	if !scheduled || len(enqueuer.tasks) != 1 {
		t.Fatalf("scheduleLockConflictFollowUp() = %v, tasks=%d; want true, 1", scheduled, len(enqueuer.tasks))
	}
	if !hasAsynqOption(enqueuer.opts[0], asynq.UniqueOpt) {
		t.Fatal("lock-conflict replacement must carry Unique")
	}
	if delay, ok := optionDuration(enqueuer.opts[0], asynq.ProcessInOpt); !ok || delay != wikiLockConflictDelay {
		t.Fatalf("lock-conflict delay = %v (ok=%v), want %v", delay, ok, wikiLockConflictDelay)
	}
	var decoded WikiIngestPayload
	if err := json.Unmarshal(enqueuer.tasks[0].Payload(), &decoded); err != nil {
		t.Fatalf("decode replacement payload: %v", err)
	}
	if decoded.TenantID != payload.TenantID ||
		decoded.KnowledgeBaseID != payload.KnowledgeBaseID ||
		decoded.WakePhase != 1 {
		t.Fatalf("replacement payload = %+v, want identity %+v phase=1", decoded, payload)
	}

	enqueuer.tasks = nil
	enqueuer.opts = nil
	payload.WakePhase = 1
	scheduled, err = svc.scheduleLockConflictFollowUp(context.Background(), payload)
	require.NoError(t, err)
	require.True(t, scheduled)
	require.Len(t, enqueuer.tasks, 1)
	decoded = WikiIngestPayload{}
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &decoded))
	require.Equal(t, uint8(0), decoded.WakePhase)
}

func TestWikiCommitProviderRetryIsSpreadAfterCircuitBoundary(t *testing.T) {
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{
		pendingRepo: &wikiQueuePendingRepoStub{},
		task:        enqueuer,
	}
	payload := WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"}
	base := 5 * time.Minute
	providerErr := &modeladmission.CircuitOpenError{
		Kind:       modeladmission.KindChat,
		RetryAfter: base,
	}

	scheduled, err := svc.scheduleProviderCircuitFollowUp(
		context.Background(), payload, providerErr,
	)
	require.NoError(t, err)
	require.True(t, scheduled)
	require.Len(t, enqueuer.tasks, 1)
	delay, ok := optionDuration(enqueuer.opts[0], asynq.ProcessInOpt)
	require.True(t, ok)
	require.GreaterOrEqual(t, delay, base)
	require.Less(t, delay, base+modeladmission.ProviderRetrySpreadWindow(base))
	require.Equal(t, delay, modeladmission.SpreadProviderRetry(
		base,
		"wiki-commit\x0042\x00kb-1",
	))
}

func TestScheduleLockConflictFollowUpEnqueueFailureIsVisible(t *testing.T) {
	enqueueErr := errors.New("redis unavailable")
	svc := &wikiIngestService{task: &wikiQueueTaskEnqueuerStub{err: enqueueErr}}

	scheduled, err := svc.scheduleLockConflictFollowUp(
		context.Background(),
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-locked"},
	)
	if scheduled || !errors.Is(err, enqueueErr) {
		t.Fatalf("scheduleLockConflictFollowUp() = (%v, %v), want (false, wrapped enqueue error)", scheduled, err)
	}
}

func TestScheduleLockConflictFollowUpConcurrentRedisContractCoalesces(t *testing.T) {
	if os.Getenv("WEKNORA_WIKI_TRIGGER_REDIS_CONTRACT") != "1" {
		t.Skip("set WEKNORA_WIKI_TRIGGER_REDIS_CONTRACT=1 to run Redis contract")
	}
	opt := asynq.RedisClientOpt{
		Addr:     os.Getenv("REDIS_ADDR"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       13,
	}
	redisClient := redis.NewClient(&redis.Options{
		Addr:     opt.Addr,
		Password: opt.Password,
		DB:       opt.DB,
	})
	require.NoError(t, redisClient.FlushDB(context.Background()).Err())
	t.Cleanup(func() {
		_ = redisClient.FlushDB(context.Background()).Err()
		_ = redisClient.Close()
	})

	client := asynq.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })
	svc := &wikiIngestService{task: client}
	payload := WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-thundering-herd"}

	const contenders = 128
	var wg sync.WaitGroup
	errs := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scheduled, err := svc.scheduleLockConflictFollowUp(context.Background(), payload)
			if err != nil {
				errs <- err
				return
			}
			if !scheduled {
				errs <- errors.New("lock-conflict successor was not acknowledged")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	inspector := asynq.NewInspector(opt)
	t.Cleanup(func() { _ = inspector.Close() })
	tasks, err := inspector.ListScheduledTasks("low", asynq.PageSize(contenders))
	require.NoError(t, err)
	require.Len(t, tasks, 1, "all contenders must collapse to one delayed signal")
	var decoded WikiIngestPayload
	require.NoError(t, json.Unmarshal(tasks[0].Payload, &decoded))
	require.Equal(t, uint8(1), decoded.WakePhase)
}

func TestSettleWikiQueueDeleteFailureReturnsErrorAndStillSchedulesRecovery(t *testing.T) {
	deleteErr := errors.New("delete transaction aborted")
	repo := &wikiQueuePendingRepoStub{deleteErr: deleteErr, pendingCount: 5}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &wikiIngestService{pendingRepo: repo, task: enqueuer}

	followUp, err := svc.settleWikiQueue(
		context.Background(),
		context.Background(),
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"},
		[]int64{1, 2, 3, 4, 5},
		nil,
	)
	if !errors.Is(err, deleteErr) {
		t.Fatalf("settleWikiQueue() error = %v, want wrapped delete error", err)
	}
	if !followUp {
		t.Fatal("delete failure should still schedule a recovery trigger")
	}
	if len(enqueuer.tasks) != 1 {
		t.Fatalf("follow-up attempts = %d, want 1", len(enqueuer.tasks))
	}
}

func TestSettleWikiQueueFollowUpFailureReturnsError(t *testing.T) {
	enqueueErr := errors.New("redis write failed")
	repo := &wikiQueuePendingRepoStub{pendingCount: 2}
	enqueuer := &wikiQueueTaskEnqueuerStub{err: enqueueErr}
	svc := &wikiIngestService{pendingRepo: repo, task: enqueuer}

	followUp, err := svc.settleWikiQueue(
		context.Background(),
		context.Background(),
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"},
		nil,
		nil,
	)
	if !errors.Is(err, enqueueErr) {
		t.Fatalf("settleWikiQueue() error = %v, want wrapped enqueue error", err)
	}
	if followUp {
		t.Fatal("followUp = true after enqueue failure")
	}
}

func TestPeekPendingListRepositoryFailureReturnsError(t *testing.T) {
	peekErr := errors.New("query timeout")
	svc := &wikiIngestService{pendingRepo: &wikiQueuePendingRepoStub{peekErr: peekErr}}

	ops, ids, err := svc.peekPendingList(context.Background(), "kb-1", 5)
	if !errors.Is(err, peekErr) {
		t.Fatalf("peekPendingList() error = %v, want wrapped query error", err)
	}
	if ops != nil || ids != nil {
		t.Fatalf("peekPendingList() = (%v, %v), want nil results on failure", ops, ids)
	}
}

func TestProcessWikiIngestRedisLockFailureIsFailClosed(t *testing.T) {
	redisClient := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
		MaxRetries:   0,
	})
	defer redisClient.Close()

	repo := &wikiQueuePendingRepoStub{}
	svc := &wikiIngestService{redisClient: redisClient, pendingRepo: repo}
	payload, err := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})
	if err != nil {
		t.Fatalf("marshal task payload: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err = svc.ProcessWikiIngest(ctx, asynq.NewTask(types.TypeWikiIngest, payload))
	if err == nil || !strings.Contains(err.Error(), "acquire active lock") {
		t.Fatalf("ProcessWikiIngest() error = %v, want active-lock acquisition failure", err)
	}
	if repo.peekCalls != 0 || repo.deleteCalls != 0 || repo.countCalls != 0 {
		t.Fatalf("lock failure must not touch queue: peek=%d delete=%d count=%d",
			repo.peekCalls, repo.deleteCalls, repo.countCalls)
	}
}

func TestRequeueFailedOpsDeadLetterFailureKeepsPendingRow(t *testing.T) {
	archiveErr := errors.New("archive insert failed")
	pending := &wikiQueuePendingRepoStub{
		incrCount:  workretry.DefaultWikiMaxAttempts,
		archiveErr: archiveErr,
	}
	svc := &wikiIngestService{pendingRepo: pending}

	err := svc.requeueFailedOps(
		context.Background(),
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"},
		[]WikiPendingOp{{Op: WikiOpIngest, KnowledgeID: "knowledge-1", dbID: 99}},
	)
	if !errors.Is(err, archiveErr) {
		t.Fatalf("requeueFailedOps() error = %v, want wrapped archive error", err)
	}
	if len(pending.deletedIDs) != 0 || len(pending.archivedIDs) != 0 {
		t.Fatalf("failed atomic archive mutated pending row: deletes=%v archives=%v", pending.deletedIDs, pending.archivedIDs)
	}
}

func TestProcessWikiIngestWikiDisabledDrainsAllPendingBatches(t *testing.T) {
	pending := &wikiQueuePendingRepoStub{
		rows:          wikiPendingRows(7),
		countFromRows: true,
	}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	kbService := &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
		ID: "kb-1",
		WikiConfig: &types.WikiConfig{
			IngestBatchSize: 5,
		},
		IndexingStrategy: types.IndexingStrategy{WikiEnabled: false},
	}}
	svc := &wikiIngestService{
		kbService:   kbService,
		pendingRepo: pending,
		task:        enqueuer,
	}
	payload, err := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})
	if err != nil {
		t.Fatalf("marshal task payload: %v", err)
	}

	if err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload)); err != nil {
		t.Fatalf("first disabled-Wiki drain error = %v", err)
	}
	if len(pending.rows) != 2 {
		t.Fatalf("rows after first terminal batch = %d, want 2", len(pending.rows))
	}
	if len(pending.deletedIDs) != 1 || !slices.Equal(pending.deletedIDs[0], []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("first terminal delete = %v, want [[1 2 3 4 5]]", pending.deletedIDs)
	}
	if len(enqueuer.tasks) != 1 {
		t.Fatalf("follow-up tasks after first batch = %d, want 1", len(enqueuer.tasks))
	}
	if !hasAsynqOption(enqueuer.opts[0], asynq.UniqueOpt) {
		t.Fatal("terminal-drain follow-up must use Unique to coalesce concurrent wake-ups")
	}
	var followUpPayload WikiIngestPayload
	if err := json.Unmarshal(enqueuer.tasks[0].Payload(), &followUpPayload); err != nil {
		t.Fatalf("decode terminal-drain follow-up: %v", err)
	}
	if followUpPayload.WakePhase != 1 {
		t.Fatalf("terminal-drain follow-up wake phase = %d, want 1", followUpPayload.WakePhase)
	}

	// Execute the scheduled follow-up directly. The second terminal batch must
	// consume the remainder and stop scheduling, proving the recovery scanner
	// cannot keep waking an immortal disabled-Wiki queue.
	if err := svc.ProcessWikiIngest(context.Background(), enqueuer.tasks[0]); err != nil {
		t.Fatalf("second disabled-Wiki drain error = %v", err)
	}
	if len(pending.rows) != 0 {
		t.Fatalf("rows after final terminal batch = %d, want 0", len(pending.rows))
	}
	if len(pending.deletedIDs) != 2 || !slices.Equal(pending.deletedIDs[1], []int64{6, 7}) {
		t.Fatalf("terminal deletes = %v, want second delete [6 7]", pending.deletedIDs)
	}
	if len(enqueuer.tasks) != 1 {
		t.Fatalf("final terminal batch scheduled another task: total=%d", len(enqueuer.tasks))
	}
	if kbService.calls != 2 {
		t.Fatalf("KB lookups = %d, want 2", kbService.calls)
	}
}

func TestProcessWikiIngestKnowledgeBaseNotFoundDrainsOrphanRows(t *testing.T) {
	pending := &wikiQueuePendingRepoStub{
		rows:          wikiPendingRows(3),
		countFromRows: true,
	}
	kbService := &wikiQueueKBServiceStub{err: apprepo.ErrKnowledgeBaseNotFound}
	svc := &wikiIngestService{
		kbService:   kbService,
		pendingRepo: pending,
		task:        &wikiQueueTaskEnqueuerStub{},
	}
	payload, err := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "deleted-kb"})
	if err != nil {
		t.Fatalf("marshal task payload: %v", err)
	}

	if err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload)); err != nil {
		t.Fatalf("not-found terminal drain error = %v", err)
	}
	if len(pending.rows) != 0 {
		t.Fatalf("orphan pending rows = %d, want 0", len(pending.rows))
	}
	if len(pending.deletedIDs) != 1 || !slices.Equal(pending.deletedIDs[0], []int64{1, 2, 3}) {
		t.Fatalf("orphan rows deleted = %v, want [[1 2 3]]", pending.deletedIDs)
	}
}

func TestProcessWikiIngestKnowledgeBaseLookupTransientErrorRetriesWithoutQueueMutation(t *testing.T) {
	lookupErr := errors.New("database connection reset")
	pending := &wikiQueuePendingRepoStub{
		rows:          wikiPendingRows(3),
		countFromRows: true,
	}
	svc := &wikiIngestService{
		kbService:   &wikiQueueKBServiceStub{err: lookupErr},
		pendingRepo: pending,
		task:        &wikiQueueTaskEnqueuerStub{},
	}
	payload, err := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})
	if err != nil {
		t.Fatalf("marshal task payload: %v", err)
	}

	err = svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	if !errors.Is(err, lookupErr) {
		t.Fatalf("transient KB lookup error = %v, want wrapped lookup error", err)
	}
	if pending.peekCalls != 0 || pending.deleteCalls != 0 || pending.countCalls != 0 {
		t.Fatalf(
			"transient KB lookup failure mutated queue: peek=%d delete=%d count=%d",
			pending.peekCalls,
			pending.deleteCalls,
			pending.countCalls,
		)
	}
	if len(pending.rows) != 3 {
		t.Fatalf("pending rows after transient lookup failure = %d, want 3", len(pending.rows))
	}
}

func TestProcessWikiIngestNilKnowledgeBaseWithoutErrorKeepsPendingRows(t *testing.T) {
	pending := &wikiQueuePendingRepoStub{
		rows:          wikiPendingRows(1),
		countFromRows: true,
	}
	svc := &wikiIngestService{
		kbService:   &wikiQueueKBServiceStub{},
		pendingRepo: pending,
		task:        &wikiQueueTaskEnqueuerStub{},
	}
	payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

	err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	if err == nil || !strings.Contains(err.Error(), "returned nil without error") {
		t.Fatalf("ProcessWikiIngest() error = %v, want nil-KB invariant error", err)
	}
	if pending.peekCalls != 0 || pending.deleteCalls != 0 || len(pending.rows) != 1 {
		t.Fatalf("nil-KB invariant mutated queue: peek=%d delete=%d rows=%d",
			pending.peekCalls, pending.deleteCalls, len(pending.rows))
	}
}

func TestIsKnowledgeGoneOnlyTreatsExplicitTerminalStatesAsGone(t *testing.T) {
	t.Run("repository not found", func(t *testing.T) {
		svc := &wikiIngestService{knowledgeSvc: &wikiQueueKnowledgeServiceStub{err: apprepo.ErrKnowledgeNotFound}}
		gone, err := svc.isKnowledgeGone(context.Background(), "kb-1", "knowledge-1")
		if err != nil || !gone {
			t.Fatalf("isKnowledgeGone() = (%v, %v), want (true, nil)", gone, err)
		}
	})

	t.Run("deleting status", func(t *testing.T) {
		svc := &wikiIngestService{knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
			ID:          "knowledge-1",
			ParseStatus: types.ParseStatusDeleting,
		}}}
		gone, err := svc.isKnowledgeGone(context.Background(), "kb-1", "knowledge-1")
		if err != nil || !gone {
			t.Fatalf("isKnowledgeGone() = (%v, %v), want (true, nil)", gone, err)
		}
	})

	t.Run("transient lookup failure", func(t *testing.T) {
		lookupErr := errors.New("postgres connection reset")
		svc := &wikiIngestService{knowledgeSvc: &wikiQueueKnowledgeServiceStub{err: lookupErr}}
		gone, err := svc.isKnowledgeGone(context.Background(), "kb-1", "knowledge-1")
		if gone || !errors.Is(err, lookupErr) {
			t.Fatalf("isKnowledgeGone() = (%v, %v), want (false, wrapped lookup error)", gone, err)
		}
	})

	t.Run("moved to another knowledge base", func(t *testing.T) {
		svc := &wikiIngestService{knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
			ID: "knowledge-1", KnowledgeBaseID: "kb-target", ParseStatus: types.ParseStatusCompleted,
		}}}
		gone, err := svc.isKnowledgeGone(context.Background(), "kb-source", "knowledge-1")
		if err != nil || !gone {
			t.Fatalf("isKnowledgeGone() = (%v, %v), want moved-away source to be gone", gone, err)
		}
	})
}

func TestPrepareWikiRetractAuthorizesOnlyCommittedMoveAwayFromSource(t *testing.T) {
	for _, tc := range []struct {
		name      string
		currentKB string
		wantApply bool
	}{
		{name: "committed target", currentKB: "kb-target", wantApply: true},
		{name: "later chained move", currentKB: "kb-third", wantApply: true},
		{name: "failed move compensated to source", currentKB: "kb-source", wantApply: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &wikiIngestService{
				knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
					ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: tc.currentKB,
					ParseStatus: types.ParseStatusCompleted,
				}},
				chunkRepo: &wikiQueueChunkRepoStub{ids: []string{"chunk-1"}},
			}
			prepared, apply, err := svc.prepareWikiRetract(context.Background(), 42, "kb-source", WikiPendingOp{
				Op: WikiOpRetract, KnowledgeID: "knowledge-1",
				MoveTargetKnowledgeBaseID: "kb-target",
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantApply, apply)
			if tc.wantApply {
				assert.Equal(t, []string{"chunk-1"}, prepared.SourceChunks)
			} else {
				assert.Empty(t, prepared.SourceChunks)
			}
		})
	}
}

func TestReduceSlugUpdatesKnowledgeLookupFailureDoesNotTouchPage(t *testing.T) {
	lookupErr := errors.New("knowledge query timeout")
	pages := &wikiQueuePageServiceStub{}
	svc := &wikiIngestService{
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{err: lookupErr},
		wikiService:  pages,
	}

	_, _, _, err := svc.reduceSlugUpdates(
		context.Background(),
		nil,
		"kb-1",
		"entity/acme",
		[]SlugUpdate{{
			Slug:                 "entity/acme",
			Type:                 types.WikiPageTypeEntity,
			KnowledgeID:          "knowledge-1",
			ProcessingGeneration: "test-generation",
		}},
		42,
		nil,
		nil,
	)
	if !errors.Is(err, lookupErr) {
		t.Fatalf("reduceSlugUpdates() error = %v, want wrapped knowledge lookup error", err)
	}
	if pages.getCalls != 0 || pages.createCalls != 0 {
		t.Fatalf("knowledge lookup failure touched wiki pages: get=%d create=%d", pages.getCalls, pages.createCalls)
	}
}

func TestReduceSlugUpdatesPageReadFailureIsNotTreatedAsNotFound(t *testing.T) {
	readErr := errors.New("wiki page query timeout")
	pages := &wikiQueuePageServiceStub{getErr: readErr}
	svc := &wikiIngestService{
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{ID: "knowledge-1"}},
		wikiService:  pages,
	}

	_, _, _, err := svc.reduceSlugUpdates(
		context.Background(),
		nil,
		"kb-1",
		"summary/knowledge-1",
		[]SlugUpdate{{
			Slug:                 "summary/knowledge-1",
			Type:                 "summary",
			KnowledgeID:          "knowledge-1",
			ProcessingGeneration: "test-generation",
			SourceRef:            "knowledge-1",
			DocTitle:             "Document",
			SummaryBody:          "body",
			SummaryLine:          "summary",
		}},
		42,
		nil,
		nil,
	)
	if !errors.Is(err, readErr) {
		t.Fatalf("reduceSlugUpdates() error = %v, want wrapped page read error", err)
	}
	if pages.createCalls != 0 {
		t.Fatalf("transient page read failure created %d pages, want 0", pages.createCalls)
	}
}

func TestReduceSlugUpdatesExplicitNotFoundMayCreatePage(t *testing.T) {
	pages := &wikiQueuePageServiceStub{getErr: apprepo.ErrWikiPageNotFound}
	svc := &wikiIngestService{
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{ID: "knowledge-1"}},
		wikiService:  pages,
	}

	changed, _, _, err := svc.reduceSlugUpdates(
		context.Background(),
		nil,
		"kb-1",
		"summary/knowledge-1",
		[]SlugUpdate{{
			Slug:                 "summary/knowledge-1",
			Type:                 "summary",
			KnowledgeID:          "knowledge-1",
			ProcessingGeneration: "test-generation",
			SourceRef:            "knowledge-1",
			DocTitle:             "Document",
			SummaryBody:          "body",
			SummaryLine:          "summary",
		}},
		42,
		nil,
		nil,
	)
	if err != nil || !changed {
		t.Fatalf("reduceSlugUpdates() = (changed=%v, err=%v), want (true, nil)", changed, err)
	}
	if pages.createCalls != 1 {
		t.Fatalf("explicit not-found create calls = %d, want 1", pages.createCalls)
	}
}

func TestReduceSlugUpdatesLLMFailurePropagatesInsteadOfAcknowledgingRetract(t *testing.T) {
	modelErr := errors.New("model rejected request")
	pages := &wikiQueuePageServiceStub{getPage: &types.WikiPage{
		ID:              "page-1",
		KnowledgeBaseID: "kb-1",
		Slug:            "entity/acme",
		PageType:        types.WikiPageTypeEntity,
		Status:          types.WikiPageStatusPublished,
		Content:         "existing content",
		SourceRefs:      types.StringArray{"knowledge-1", "knowledge-2|Surviving source"},
	}}
	svc := &wikiIngestService{wikiService: pages}

	_, _, _, err := svc.reduceSlugUpdates(
		context.Background(),
		&templateCaptureChatModel{err: modelErr},
		"kb-1",
		"entity/acme",
		[]SlugUpdate{{
			Slug:              "entity/acme",
			Type:              "retract",
			KnowledgeID:       "knowledge-1",
			RetractDocContent: "removed contribution",
		}},
		42,
		&WikiBatchContext{
			SlugTitleMany: func(context.Context, []string) map[string]string { return nil },
			SummaryContentByKnowledgeID: func(context.Context, string) string {
				return "surviving source summary"
			},
		},
		nil,
	)
	if !errors.Is(err, modelErr) {
		t.Fatalf("reduceSlugUpdates() error = %v, want wrapped LLM failure", err)
	}
}

func TestReduceSlugUpdatesRejectsIdentityChangedWhileLLMIsBlocked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*types.Knowledge)
	}{
		{
			name: "reparse advances generation",
			mutate: func(knowledge *types.Knowledge) {
				knowledge.ProcessingGeneration = "generation-2"
			},
		},
		{
			name: "move changes knowledge base",
			mutate: func(knowledge *types.Knowledge) {
				knowledge.KnowledgeBaseID = "kb-2"
			},
		},
		{
			name: "delete enters terminal state",
			mutate: func(knowledge *types.Knowledge) {
				knowledge.ParseStatus = types.ParseStatusDeleting
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			processedAt := time.Now().UTC()
			knowledge := &types.Knowledge{
				ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1",
				ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusCompleted,
				ProcessedAt: &processedAt,
			}
			knowledgeSvc := &wikiQueueKnowledgeServiceStub{knowledge: knowledge}
			pages := &wikiQueuePageServiceStub{getPage: &types.WikiPage{
				ID: "page-1", TenantID: 42, KnowledgeBaseID: "kb-1", Slug: "entity/acme",
				PageType: types.WikiPageTypeEntity, Status: types.WikiPageStatusPublished,
				Content: "old page body", SourceRefs: types.StringArray{"knowledge-other"},
			}}
			model := &blockingWikiChatModel{entered: make(chan struct{}), release: make(chan struct{})}
			svc := &wikiIngestService{knowledgeSvc: knowledgeSvc, wikiService: pages}
			update := SlugUpdate{
				Slug: "entity/acme", Type: types.WikiPageTypeEntity,
				Item:     extractedItem{Name: "Acme", Description: "new facts", Details: "new facts"},
				DocTitle: "Document", KnowledgeID: "knowledge-1", SourceRef: "knowledge-1",
				ProcessingGeneration: "generation-1", SourceOpID: 77, Language: "English",
			}
			batchCtx := &WikiBatchContext{
				SlugTitleMany:   func(context.Context, []string) map[string]string { return nil },
				PlannedFolderID: map[string]string{},
			}

			done := make(chan error, 1)
			go func() {
				_, _, _, err := svc.reduceSlugUpdates(
					context.Background(), model, "kb-1", update.Slug,
					[]SlugUpdate{update}, 42, batchCtx, nil,
				)
				done <- err
			}()

			select {
			case <-model.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("reduce did not enter the blocking LLM")
			}
			knowledgeSvc.mutateKnowledge(tc.mutate)
			close(model.release)
			err := <-done
			require.Error(t, err)
			require.NotEmpty(t, wikiingestguard.StaleIdentities(err))
			assert.Empty(t, pages.contentWrites)
			assert.Zero(t, pages.createCalls)
			assert.Empty(t, pages.updated)
		})
	}
}

func TestReduceSlugUpdatesRetractQuarantinesAndDoesNotApplySameOperationTwice(t *testing.T) {
	page := &types.WikiPage{
		ID:              "page-1",
		KnowledgeBaseID: "kb-1",
		Slug:            "entity/acme",
		PageType:        types.WikiPageTypeEntity,
		Status:          types.WikiPageStatusPublished,
		Content:         "deleted and surviving facts",
		SourceRefs:      types.StringArray{"knowledge-1", "knowledge-2|Surviving source"},
		ChunkRefs:       types.StringArray{"chunk-deleted", "chunk-surviving"},
	}
	pages := &wikiQueuePageServiceStub{getPage: page}
	svc := &wikiIngestService{wikiService: pages}
	update := SlugUpdate{
		Slug:              "entity/acme",
		Type:              "retract",
		KnowledgeID:       "knowledge-1",
		RetractDocContent: "deleted contribution",
		SourceChunks:      []string{"chunk-deleted"},
		SourceOpID:        77,
	}
	batchCtx := &WikiBatchContext{
		SlugTitleMany: func(context.Context, []string) map[string]string { return nil },
		SummaryContentByKnowledgeID: func(context.Context, string) string {
			return "surviving source summary"
		},
	}

	model := &templateCaptureChatModel{response: "SUMMARY: safe\nsurviving facts only"}
	changed, affected, _, err := svc.reduceSlugUpdates(
		context.Background(), model, "kb-1", page.Slug, []SlugUpdate{update}, 42, batchCtx, nil,
	)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "retract", affected)
	require.Len(t, pages.updated, 1, "quarantine must persist before the model edit")
	assert.Equal(t, types.WikiPageStatusArchived, pages.updated[0].Status)
	require.Len(t, pages.contentWrites, 1)
	assert.Equal(t, types.WikiPageStatusPublished, pages.contentWrites[0].Status)
	assert.Equal(t, types.StringArray{"knowledge-2|Surviving source"}, pages.contentWrites[0].SourceRefs)
	assert.Equal(t, types.StringArray{"chunk-surviving"}, pages.contentWrites[0].ChunkRefs)

	pages.getPage = pages.contentWrites[0]
	retryModel := &templateCaptureChatModel{err: errors.New("must not be called")}
	changed, affected, _, err = svc.reduceSlugUpdates(
		context.Background(), retryModel, "kb-1", page.Slug, []SlugUpdate{update}, 42, batchCtx, nil,
	)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, "retract", affected)
	assert.Empty(t, retryModel.prompt, "applied source_op_id must bypass the LLM on retry")
}

func TestMergeFailedWikiOpsCountsMultiSlugContributorOnceAndAcksUnaffected(t *testing.T) {
	pending := []WikiPendingOp{
		{Op: WikiOpIngest, KnowledgeID: "knowledge-a", dbID: 1},
		{Op: WikiOpIngest, KnowledgeID: "knowledge-b", dbID: 2},
		{Op: WikiOpIngest, KnowledgeID: "knowledge-c", dbID: 3},
	}
	existing := []WikiPendingOp{pending[0]}
	failedKnowledgeIDs := map[string]struct{}{
		"knowledge-a": {}, // contributed to two failed slugs
		"knowledge-b": {},
	}

	failed := mergeFailedWikiOps(existing, pending, failedKnowledgeIDs)
	if len(failed) != 2 || failed[0].dbID != 1 || failed[1].dbID != 2 {
		t.Fatalf("merged failed ops = %+v, want db IDs [1 2] exactly once", failed)
	}
	trimIDs := wikiQueueTrimIDs([]int64{1, 2, 3}, failed)
	if !slices.Equal(trimIDs, []int64{3}) {
		t.Fatalf("trim IDs = %v, want only unaffected row [3]", trimIDs)
	}

	repo := &wikiQueuePendingRepoStub{incrCount: 1}
	svc := &wikiIngestService{pendingRepo: repo}
	if err := svc.requeueFailedOps(context.Background(), WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"}, failed); err != nil {
		t.Fatalf("requeueFailedOps() error = %v", err)
	}
	if !slices.Equal(repo.incrementedIDs, []int64{1, 2}) {
		t.Fatalf("fail-count increments = %v, want [1 2] once each", repo.incrementedIDs)
	}
}

func TestPeekPendingListMalformedRetractFallsBackToDurableColumns(t *testing.T) {
	repo := &wikiQueuePendingRepoStub{rows: []*types.TaskPendingOp{{
		ID:       91,
		Op:       WikiOpRetract,
		DedupKey: "knowledge-1",
		Payload:  json.RawMessage(`{"broken"`),
	}}}
	svc := &wikiIngestService{pendingRepo: repo}

	ops, ids, err := svc.peekPendingList(context.Background(), "kb-1", 5)
	if err != nil {
		t.Fatalf("peekPendingList() error = %v", err)
	}
	if len(ops) != 1 || ops[0].Op != WikiOpRetract || ops[0].KnowledgeID != "knowledge-1" || ops[0].dbID != 91 {
		t.Fatalf("fallback ops = %+v, want durable retract columns", ops)
	}
	if !slices.Equal(ids, []int64{91}) {
		t.Fatalf("peeked IDs = %v, want [91]", ids)
	}
}

func TestPeekPendingListTerminalCleanupCanDrainHeldRetractIntent(t *testing.T) {
	repo := &wikiQueuePendingRepoStub{rows: []*types.TaskPendingOp{
		wikiPendingRow(92, WikiPendingOp{
			Op:               WikiOpRetract,
			KnowledgeID:      "knowledge-held",
			RetractPlanState: wikiRetractPlanIntent,
		}),
	}}
	svc := &wikiIngestService{pendingRepo: repo}

	ops, ids, err := svc.peekPendingList(context.Background(), "kb-1", 5)
	require.NoError(t, err)
	assert.Empty(t, ops)
	assert.Empty(t, ids)

	ops, ids, err = svc.peekPendingListIncludingRetractIntents(
		context.Background(), "kb-1", 5, true,
	)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.Equal(t, wikiRetractPlanIntent, ops[0].RetractPlanState)
	assert.Equal(t, []int64{92}, ids)
}

func TestWikiGenerationPreflightLateOldOpCannotSuppressCurrentGeneration(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	repo := &wikiQueuePendingRepoStub{rows: []*types.TaskPendingOp{
		wikiPendingRow(1, WikiPendingOp{
			Op: WikiOpIngest, KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-new",
		}),
		// Simulate the old PostProcess producer winning the enqueue race later.
		wikiPendingRow(2, WikiPendingOp{
			Op: WikiOpIngest, KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-old",
		}),
	}}
	svc := &wikiIngestService{
		pendingRepo: repo,
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
			ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1",
			ProcessingGeneration: "generation-new", ParseStatus: types.ParseStatusCompleted,
			ProcessedAt: &now,
		}},
	}

	ops, ids, err := svc.peekPendingList(context.Background(), "kb-1", 10)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, ids)
	require.Len(t, ops, 2, "generation-scoped dedup must retain both rows for authoritative preflight")

	processable, failed, stale := svc.preflightWikiPendingOps(context.Background(), WikiIngestPayload{
		TenantID: 42, KnowledgeBaseID: "kb-1",
	}, ops)
	require.Empty(t, failed)
	require.Equal(t, 1, stale)
	require.Len(t, processable, 1)
	require.Equal(t, "generation-new", processable[0].ProcessingGeneration)
	require.Equal(t, int64(1), processable[0].dbID)
}

func TestWikiGenerationPreflightSettledCurrentGenerationIsQueueNoop(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc := &wikiIngestService{
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
			ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1",
			ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusFinalizing,
			ProcessedAt: &now, WikiStatus: types.WikiStatusCompleted,
		}},
	}
	op := WikiPendingOp{
		Op: WikiOpIngest, KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1", tenantID: 42, dbID: 99,
	}

	processable, failed, stale := svc.preflightWikiPendingOps(
		context.Background(),
		WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"},
		[]WikiPendingOp{op},
	)
	require.Empty(t, processable)
	require.Empty(t, failed)
	require.Equal(t, 1, stale)
}

func TestProcessWikiIngestStaleGenerationIsTerminalWithoutFailCountRetry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pending := &wikiQueuePendingRepoStub{
		rows: []*types.TaskPendingOp{wikiPendingRow(19, WikiPendingOp{
			Op: WikiOpIngest, KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-old",
		})},
		countFromRows: true,
	}
	svc := &wikiIngestService{
		pendingRepo: pending,
		kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
			ID: "kb-1", TenantID: 42, WikiConfig: &types.WikiConfig{IngestBatchSize: 5},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		}},
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
			ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1",
			ProcessingGeneration: "generation-new", ParseStatus: types.ParseStatusCompleted,
			ProcessedAt: &now,
		}},
	}
	payload, err := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})
	require.NoError(t, err)

	require.NoError(t, svc.ProcessWikiIngest(
		context.Background(), asynq.NewTask(types.TypeWikiIngest, payload),
	))
	assert.Empty(t, pending.rows, "terminal stale work must be acknowledged")
	assert.Empty(t, pending.incrementedIDs, "terminal stale work must not consume the retry budget")
	assert.Empty(t, pending.archived, "terminal stale work is superseded, not a dead letter")
}

func TestRestorePreparedWikiIngestMarksOnlyDurablyAppliedSlugs(t *testing.T) {
	op := WikiPendingOp{
		Op: WikiOpIngest, KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-1",
		dbID: 77,
		Prepared: &wikiPreparedIngest{
			DocTitle: "Document", Summary: "summary",
			Pages: []types.WikiLogPageRef{{Slug: "summary/knowledge-1", Title: "Document"}},
			Updates: []SlugUpdate{
				{Slug: "summary/knowledge-1", Type: "summary"},
				{Slug: "entity/acme", Type: types.WikiPageTypeEntity},
			},
		},
		AppliedPageSlugs: []string{"entity/acme"},
	}
	svc := &wikiIngestService{}
	result, updates, restored := svc.restorePreparedWikiIngest(
		context.Background(), WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"}, op,
	)

	require.True(t, restored)
	require.NotNil(t, result)
	assert.Equal(t, int64(77), result.SourceOpID)
	require.Len(t, updates, 2)
	assert.False(t, updates[0].PageAlreadyApplied)
	assert.True(t, updates[1].PageAlreadyApplied)
	for _, update := range updates {
		assert.Equal(t, "knowledge-1", update.KnowledgeID)
		assert.Equal(t, "generation-1", update.ProcessingGeneration)
		assert.Equal(t, int64(77), update.SourceOpID)
	}
}

func TestWikiMapSubstageCheckpointSurvivesAndPreparedPlanCompactsIt(t *testing.T) {
	payload := WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"}
	op := WikiPendingOp{
		Op: WikiOpIngest, KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1", dbID: 77,
	}
	pending := &wikiQueuePendingRepoStub{
		rows: []*types.TaskPendingOp{wikiPendingRow(77, op)},
	}
	service := &wikiIngestService{pendingRepo: pending}
	checkpoint := &wikiMapCheckpoint{
		Version: wikiMapCheckpointVersion, ContentHash: "content-digest",
		ExtractionDone: true,
		ExtractedEntities: []extractedItem{{
			Name: "Acme", Slug: "entity/acme",
		}},
		SummaryDone:    true,
		SummaryContent: "SUMMARY: Acme",
	}
	require.NoError(t, service.checkpointWikiMapProgress(
		context.Background(), payload, op, checkpoint,
	))

	var resumed WikiPendingOp
	require.NoError(t, json.Unmarshal(pending.rows[0].Payload, &resumed))
	require.NotNil(t, resumed.MapCheckpoint)
	assert.True(t, resumed.MapCheckpoint.ExtractionDone)
	assert.True(t, resumed.MapCheckpoint.SummaryDone)
	assert.Nil(t, resumed.Prepared)

	resumed.dbID = 77
	require.NoError(t, service.checkpointPreparedWikiIngest(
		context.Background(),
		payload,
		resumed,
		&docIngestResult{
			KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-1",
			DocTitle: "Document", Summary: "Acme", SourceOpID: 77,
		},
		[]SlugUpdate{{Slug: "summary/knowledge-1", Type: types.WikiPageTypeSummary}},
	))
	var prepared WikiPendingOp
	require.NoError(t, json.Unmarshal(pending.rows[0].Payload, &prepared))
	require.NotNil(t, prepared.Prepared)
	assert.Nil(t, prepared.MapCheckpoint, "final map plan should compact partial substage state")
}

func TestIntersectWikiChunkRefsBySlugKeepsOnlyOwnerEvidence(t *testing.T) {
	got := intersectWikiChunkRefsBySlug(
		map[string]types.StringArray{
			"entity/shared": {"owned-old-1", "other-source", "owned-old-1", ""},
			"concept/stale": {"owned-old-2"},
			"entity/other":  {"other-source"},
		},
		[]string{"owned-old-1", "owned-old-2", "not-cited"},
	)

	assert.Equal(t, []string{"owned-old-1"}, got["entity/shared"])
	assert.Equal(t, []string{"owned-old-2"}, got["concept/stale"])
	_, retainedOtherSource := got["entity/other"]
	assert.False(t, retainedOtherSource,
		"a source document must never claim another document's page citation")
}

func TestAppendWikiReparseReconciliationCarriesPageScopedOldCitations(t *testing.T) {
	updates, overlap, stale := appendWikiReparseReconciliation(
		[]SlugUpdate{{
			Slug: "entity/shared", Type: types.WikiPageTypeEntity,
			KnowledgeID: "knowledge-1", SourceChunks: []string{"new-1"},
		}},
		map[string]bool{
			"entity/shared":        true,
			"concept/no-longer-in": true,
			"summary/knowledge-1":  true,
		},
		map[string]bool{
			"entity/shared":       true,
			"summary/knowledge-1": true,
		},
		map[string][]string{
			"entity/shared":        {"old-shared-1", "old-shared-2"},
			"concept/no-longer-in": {"old-stale"},
		},
		"previous contribution",
		"current document",
		"Document",
		"knowledge-1",
		"English",
	)

	assert.Equal(t, 1, overlap)
	assert.Equal(t, 1, stale)
	require.Len(t, updates, 3,
		"one addition, one overlap retract and one stale retract are expected")

	var overlapRetract, staleRetract *SlugUpdate
	for i := range updates {
		switch {
		case updates[i].Slug == "entity/shared" && updates[i].Type == "retract":
			overlapRetract = &updates[i]
		case updates[i].Slug == "concept/no-longer-in" && updates[i].Type == "retractStale":
			staleRetract = &updates[i]
		case strings.HasPrefix(updates[i].Slug, "summary/") && updates[i].Type == "retract":
			t.Fatalf("summary pages are wholesale replacements and must not receive a retract: %+v", updates[i])
		}
	}
	require.NotNil(t, overlapRetract)
	assert.Equal(t, []string{"old-shared-1", "old-shared-2"}, overlapRetract.SourceChunks)
	assert.Equal(t, "previous contribution", overlapRetract.RetractDocContent)
	require.NotNil(t, staleRetract)
	assert.Equal(t, []string{"old-stale"}, staleRetract.SourceChunks)
	assert.Equal(t, "current document", staleRetract.RetractDocContent)
}

func TestWikiReparseChunkRefsReplaceOnlyTheCurrentDocumentGeneration(t *testing.T) {
	current := types.StringArray{
		"other-source-1",
		"old-owned-1",
		"old-owned-2",
		"other-source-2",
		"old-owned-1",
	}
	retracts := []SlugUpdate{{
		Type:         "retract",
		KnowledgeID:  "knowledge-1",
		SourceChunks: []string{"old-owned-1", "old-owned-2"},
	}}
	additions := []SlugUpdate{{
		Type:         types.WikiPageTypeEntity,
		KnowledgeID:  "knowledge-1",
		SourceChunks: []string{"new-owned-1", "new-owned-2", "new-owned-1"},
	}}

	replaced := mergeChunkRefs(
		removeChunkRefsFromRetracts(current, retracts),
		additions,
	)
	assert.Equal(t, types.StringArray{
		"other-source-1",
		"other-source-2",
		"new-owned-1",
		"new-owned-2",
	}, replaced)

	// A durable replay after the same replacement is idempotent: no old
	// generation reappears and no new citation is duplicated.
	replayed := mergeChunkRefs(
		removeChunkRefsFromRetracts(replaced, retracts),
		additions,
	)
	assert.Equal(t, replaced, replayed)
}

func TestReduceSlugUpdatesDurablyAppliedPageSkipsLLMAndPageMutation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pages := &wikiQueuePageServiceStub{getPage: &types.WikiPage{
		ID: "page-1", TenantID: 42, KnowledgeBaseID: "kb-1", Slug: "entity/acme",
		PageType: types.WikiPageTypeEntity, Status: types.WikiPageStatusDraft,
		Content: "already committed body", SourceRefs: types.StringArray{"knowledge-1"},
	}}
	model := &templateCaptureChatModel{err: errors.New("must not be called")}
	svc := &wikiIngestService{
		wikiService: pages,
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
			ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1",
			ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusCompleted,
			ProcessedAt: &now,
		}},
	}
	changed, affected, _, err := svc.reduceSlugUpdates(
		context.Background(), model, "kb-1", "entity/acme", []SlugUpdate{{
			Slug: "entity/acme", Type: types.WikiPageTypeEntity,
			KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-1",
			SourceOpID: 77, PageAlreadyApplied: true,
		}}, 42, &WikiBatchContext{}, nil,
	)

	require.NoError(t, err)
	assert.True(t, changed, "downstream publication/log/index stages still need replay")
	assert.Equal(t, "ingest", affected)
	assert.Empty(t, model.prompt)
	assert.Zero(t, pages.createCalls)
	assert.Empty(t, pages.contentWrites)
	assert.Empty(t, pages.updated)
}

func TestProcessWikiIngestReplayAfterPostPageStageFailureIsExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name               string
		stage              string
		firstRunReturnsErr bool
		wantModelCalls     int
	}{
		{name: "log response fails after commit", stage: "log", wantModelCalls: 1},
		{name: "index write fails", stage: "index", wantModelCalls: 2},
		{name: "publication write fails", stage: "publication", wantModelCalls: 1},
		{name: "queue settlement delete fails", stage: "settlement", firstRunReturnsErr: true, wantModelCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const summarySlug = "summary/knowledge-1"
			now := time.Unix(1_700_000_000, 0)
			op := WikiPendingOp{
				Op: WikiOpIngest, KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-1",
				Language: "en-US",
				Prepared: &wikiPreparedIngest{
					DocTitle: "Document", Summary: "document summary",
					Pages: []types.WikiLogPageRef{{Slug: summarySlug, Title: "Document - Summary"}},
					Updates: []SlugUpdate{{
						Slug: summarySlug, Type: "summary", DocTitle: "Document",
						SourceRef: "knowledge-1", SummaryBody: "durable summary body",
						SummaryLine: "durable summary line",
					}},
				},
			}
			pending := &wikiQueuePendingRepoStub{
				rows:          []*types.TaskPendingOp{wikiPendingRow(77, op)},
				countFromRows: true,
			}
			pages := &wikiReplayPageServiceStub{
				pending: pending,
				pages: map[string]*types.WikiPage{summarySlug: {
					ID: "summary-page", TenantID: 42, KnowledgeBaseID: "kb-1", Slug: summarySlug,
					Title: "Old", PageType: types.WikiPageTypeSummary, Status: types.WikiPageStatusDraft,
					Content: "old summary body", Version: 1,
				}},
				index: &types.WikiPage{
					ID: "index-page", TenantID: 42, KnowledgeBaseID: "kb-1", Slug: "index",
					Title: "Index", PageType: types.WikiPageTypeIndex, Status: types.WikiPageStatusPublished,
					Content: "existing index intro", Summary: "existing index intro", Version: 1,
				},
				contentWritesBySlug:     make(map[string]int),
				publicationWritesBySlug: make(map[string]int),
			}
			logs := &wikiReplayLogEntryServiceStub{bySourceOpID: make(map[int64]*types.WikiLogEntry)}
			switch tc.stage {
			case "log":
				logs.failuresAfterCommit = 1
			case "index":
				pages.indexWriteFailures = 1
			case "publication":
				pages.publicationWriteFailures = 1
			case "settlement":
				pending.deleteErr = errors.New("injected queue settlement failure")
			default:
				t.Fatalf("unknown stage %q", tc.stage)
			}
			model := &countingWikiChatModel{response: "updated index intro"}
			svc := &wikiIngestService{
				wikiService: pages,
				kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
					ID: "kb-1", TenantID: 42,
					WikiConfig: &types.WikiConfig{
						SynthesisModelID: "model-1", IngestBatchSize: 1,
						IngestMapParallel: 1, IngestReduceParallel: 1,
					},
					IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
				}},
				knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
					ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1",
					ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusCompleted,
					ProcessedAt: &now,
				}},
				modelService: &wikiQueueModelServiceStub{model: model},
				logEntrySvc:  logs,
				pendingRepo:  pending,
				task:         &wikiQueueTaskEnqueuerStub{},
			}
			payload, err := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})
			require.NoError(t, err)
			task := asynq.NewTask(types.TypeWikiIngest, payload)

			firstErr := svc.ProcessWikiIngest(context.Background(), task)
			if tc.firstRunReturnsErr {
				require.Error(t, firstErr)
			} else {
				require.NoError(t, firstErr)
			}
			require.Len(t, pending.rows, 1, "failed post-page stage must keep the durable operation")
			var afterFirst WikiPendingOp
			require.NoError(t, json.Unmarshal(pending.rows[0].Payload, &afterFirst))
			require.NotNil(t, afterFirst.Prepared)
			assert.Equal(t, []string{summarySlug}, afterFirst.AppliedPageSlugs)
			assert.Equal(t, 1, pages.contentWritesBySlug[summarySlug],
				"the content page must have committed before the injected failure")

			pending.deleteErr = nil
			require.NoError(t, svc.ProcessWikiIngest(context.Background(), task))

			assert.Empty(t, pending.rows)
			assert.Zero(t, pending.checkpointCalls,
				"a durable Prepared plan must bypass Map and never be checkpointed again")
			assert.Equal(t, 1, pages.contentWritesBySlug[summarySlug],
				"AppliedPageSlugs must suppress duplicate content mutation on replay")
			assert.Equal(t, types.WikiPageStatusPublished, pages.pages[summarySlug].Status)
			assert.Equal(t, "durable summary body", pages.pages[summarySlug].Content)
			assert.Len(t, logs.bySourceOpID, 1, "source_op_id makes log replay idempotent")
			assert.Equal(t, tc.wantModelCalls, model.callCount())
		})
	}
}

func TestProcessWikiIngestWikiDisabledReconcilesRetractBeforeAck(t *testing.T) {
	pending := &wikiQueuePendingRepoStub{
		rows: []*types.TaskPendingOp{wikiPendingRow(1, WikiPendingOp{
			Op:           WikiOpRetract,
			KnowledgeID:  "knowledge-1",
			PageSlugs:    []string{"entity/sole", "entity/shared"},
			SourceChunks: []string{"chunk-old"},
		})},
		countFromRows: true,
	}
	indexPage := &types.WikiPage{
		Slug: "index", PageType: types.WikiPageTypeIndex, Status: types.WikiPageStatusPublished,
	}
	sharedPage := &types.WikiPage{
		Slug: "entity/shared", PageType: types.WikiPageTypeEntity, Status: types.WikiPageStatusPublished,
		SourceRefs: types.StringArray{"knowledge-1|Document", "knowledge-2"},
		ChunkRefs:  types.StringArray{"chunk-old", "chunk-surviving"},
	}
	require.NoError(t, wikidelete.Quarantine(indexPage, "knowledge-1", "knowledge-concurrent"))
	require.NoError(t, wikidelete.Quarantine(sharedPage, "knowledge-1", "knowledge-concurrent"))
	pages := &wikiQueuePageServiceStub{
		indexPage: indexPage,
		listPages: []*types.WikiPage{
			{Slug: "entity/sole", PageType: types.WikiPageTypeEntity, SourceRefs: types.StringArray{"knowledge-1"}, ChunkRefs: types.StringArray{"chunk-old"}},
			sharedPage,
		},
	}
	logs := &wikiQueueLogEntryServiceStub{}
	svc := &wikiIngestService{
		kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			WikiConfig:       &types.WikiConfig{IngestBatchSize: 5},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: false},
		}},
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
			ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusDeleting,
		}},
		chunkRepo:   &wikiQueueChunkRepoStub{ids: []string{"chunk-db"}},
		wikiService: pages,
		logEntrySvc: logs,
		pendingRepo: pending,
		task:        &wikiQueueTaskEnqueuerStub{},
	}
	payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

	if err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload)); err != nil {
		t.Fatalf("disabled-Wiki retract drain error = %v", err)
	}
	if !slices.Equal(pages.deleted, []string{"entity/sole"}) {
		t.Fatalf("deleted pages = %v, want [entity/sole]", pages.deleted)
	}
	if len(pages.updated) != 1 || pages.updated[0].Slug != "entity/shared" ||
		!slices.Equal(pages.updated[0].SourceRefs, types.StringArray{"knowledge-2"}) ||
		!slices.Equal(pages.updated[0].ChunkRefs, types.StringArray{"chunk-surviving"}) ||
		pages.updated[0].Status != types.WikiPageStatusArchived {
		t.Fatalf("shared-page metadata updates = %+v, want archived source/chunk refs for surviving source", pages.updated)
	}
	if len(pages.contentWrites) != 1 || pages.contentWrites[0].PageType != types.WikiPageTypeIndex {
		t.Fatalf("disabled-Wiki index resets = %+v, want one neutral index write", pages.contentWrites)
	}
	assert.Equal(t, [][]string{{"knowledge-1"}}, pages.metaClearIDs)
	assert.Equal(t, [][]string{{"knowledge-1"}}, pages.contentClearIDs)
	sharedPending, err := wikidelete.PendingSources(pages.updated[0])
	require.NoError(t, err)
	assert.Equal(t, []string{"knowledge-concurrent"}, sharedPending,
		"completing one retract must retain a concurrent page quarantine")
	indexPending, err := wikidelete.PendingSources(pages.contentWrites[0])
	require.NoError(t, err)
	assert.Equal(t, []string{"knowledge-concurrent"}, indexPending,
		"completing one retract must retain a concurrent index quarantine")
	assert.Equal(t, types.WikiPageStatusArchived, pages.contentWrites[0].Status)
	sharedApplied, err := wikidelete.IsApplied(pages.updated[0], 1)
	require.NoError(t, err)
	assert.True(t, sharedApplied)
	indexApplied, err := wikidelete.IsApplied(pages.contentWrites[0], 1)
	require.NoError(t, err)
	assert.True(t, indexApplied)
	if len(logs.entries) != 1 || logs.entries[0].Action != "retract" ||
		logs.entries[0].SourceOpID == nil || *logs.entries[0].SourceOpID != 1 {
		t.Fatalf("disabled-Wiki log entries = %+v, want idempotent retract event for op 1", logs.entries)
	}
	assert.Empty(t, logs.entries[0].DocTitle)
	assert.Empty(t, logs.entries[0].Summary)
	assert.Empty(t, logs.entries[0].PagesAffected)
	if len(pending.rows) != 0 || !slices.Equal(pending.deletedIDs[0], []int64{1}) {
		t.Fatalf("pending queue after deterministic retract = rows:%d deletes:%v", len(pending.rows), pending.deletedIDs)
	}
}

func TestReconcileDisabledWikiRetractCompletesMarkerAfterPriorSourceRefCleanup(t *testing.T) {
	page := &types.WikiPage{
		Slug:       "entity/shared",
		PageType:   types.WikiPageTypeEntity,
		Status:     types.WikiPageStatusPublished,
		SourceRefs: types.StringArray{"knowledge-surviving"},
	}
	require.NoError(t, wikidelete.Quarantine(page, "knowledge-removed", "knowledge-concurrent"))
	pages := &wikiQueuePageServiceStub{getPage: page}
	svc := &wikiIngestService{wikiService: pages}

	require.NoError(t, svc.reconcileDisabledWikiRetract(context.Background(), "kb-1", WikiPendingOp{
		Op:          WikiOpRetract,
		KnowledgeID: "knowledge-removed",
		PageSlugs:   []string{"entity/shared"},
		dbID:        91,
	}))

	require.Len(t, pages.updated, 1)
	assert.Equal(t, types.WikiPageStatusArchived, pages.updated[0].Status)
	assert.Equal(t, [][]string{{"knowledge-removed"}}, pages.metaClearIDs)
	pendingSources, err := wikidelete.PendingSources(pages.updated[0])
	require.NoError(t, err)
	assert.Equal(t, []string{"knowledge-concurrent"}, pendingSources)
	applied, err := wikidelete.IsApplied(pages.updated[0], 91)
	require.NoError(t, err)
	assert.True(t, applied)
}

func TestProcessWikiIngestWikiDisabledSharedMarkerOrWriteFailureKeepsRetractPending(t *testing.T) {
	for _, tc := range []struct {
		name      string
		page      func(t *testing.T) *types.WikiPage
		updateErr error
	}{
		{
			name: "invalid marker",
			page: func(*testing.T) *types.WikiPage {
				return &types.WikiPage{
					Slug: "entity/shared", PageType: types.WikiPageTypeEntity,
					SourceRefs:   types.StringArray{"knowledge-1", "knowledge-2"},
					PageMetadata: types.JSON(`{"_weknora_delete_quarantine":`),
				}
			},
		},
		{
			name: "metadata write",
			page: func(t *testing.T) *types.WikiPage {
				page := &types.WikiPage{
					Slug: "entity/shared", PageType: types.WikiPageTypeEntity, Status: types.WikiPageStatusPublished,
					SourceRefs: types.StringArray{"knowledge-1", "knowledge-2"},
				}
				require.NoError(t, wikidelete.Quarantine(page, "knowledge-1", "knowledge-concurrent"))
				return page
			},
			updateErr: errors.New("wiki metadata write unavailable"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := &wikiQueuePendingRepoStub{
				rows: []*types.TaskPendingOp{wikiPendingRow(31, WikiPendingOp{
					Op: WikiOpRetract, KnowledgeID: "knowledge-1", PageSlugs: []string{"entity/shared"},
				})},
				countFromRows: true,
			}
			pages := &wikiQueuePageServiceStub{listPages: []*types.WikiPage{tc.page(t)}, updateErr: tc.updateErr}
			svc := &wikiIngestService{
				kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
					ID:               "kb-1",
					WikiConfig:       &types.WikiConfig{IngestBatchSize: 5},
					IndexingStrategy: types.IndexingStrategy{WikiEnabled: false},
				}},
				knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
					ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusDeleting,
				}},
				chunkRepo:   &wikiQueueChunkRepoStub{},
				wikiService: pages,
				pendingRepo: pending,
				task:        &wikiQueueTaskEnqueuerStub{},
			}
			payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

			require.NoError(t, svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload)))
			assert.Len(t, pending.rows, 1, "failed shared-page completion must leave the durable retract pending")
			assert.Empty(t, pending.deletedIDs)
			assert.Equal(t, []int64{31}, pending.incrementedIDs)
		})
	}
}

func TestProcessWikiIngestWikiDisabledIndexMarkerOrWriteFailureKeepsRetractPending(t *testing.T) {
	for _, tc := range []struct {
		name      string
		indexPage *types.WikiPage
		updateErr error
	}{
		{
			name: "invalid marker",
			indexPage: &types.WikiPage{
				Slug: "index", PageType: types.WikiPageTypeIndex, Status: types.WikiPageStatusPublished,
				PageMetadata: types.JSON(`{"_weknora_delete_quarantine":`),
			},
		},
		{
			name: "content write",
			indexPage: &types.WikiPage{
				Slug: "index", PageType: types.WikiPageTypeIndex, Status: types.WikiPageStatusPublished,
			},
			updateErr: errors.New("wiki index write unavailable"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := &wikiQueuePendingRepoStub{
				rows: []*types.TaskPendingOp{wikiPendingRow(41, WikiPendingOp{
					Op:          WikiOpRetract,
					KnowledgeID: "knowledge-1",
				})},
				countFromRows: true,
			}
			svc := &wikiIngestService{
				kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
					ID:               "kb-1",
					WikiConfig:       &types.WikiConfig{IngestBatchSize: 5},
					IndexingStrategy: types.IndexingStrategy{WikiEnabled: false},
				}},
				knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
					ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusDeleting,
				}},
				chunkRepo:   &wikiQueueChunkRepoStub{},
				wikiService: &wikiQueuePageServiceStub{indexPage: tc.indexPage, updateErr: tc.updateErr},
				logEntrySvc: &wikiQueueLogEntryServiceStub{},
				pendingRepo: pending,
				task:        &wikiQueueTaskEnqueuerStub{},
			}
			payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

			require.NoError(t, svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload)))
			assert.Len(t, pending.rows, 1, "failed index completion must leave the durable retract pending")
			assert.Empty(t, pending.deletedIDs)
			assert.Equal(t, []int64{41}, pending.incrementedIDs)
		})
	}
}

func TestProcessWikiIngestWikiDisabledRetractFailureDeadLettersWithCause(t *testing.T) {
	cleanupErr := errors.New("wiki page database unavailable")
	pending := &wikiQueuePendingRepoStub{
		rows: []*types.TaskPendingOp{wikiPendingRow(1, WikiPendingOp{
			Op:          WikiOpRetract,
			KnowledgeID: "knowledge-1",
		})},
		countFromRows: true,
		incrCount:     workretry.DefaultWikiMaxAttempts,
	}
	svc := &wikiIngestService{
		kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			WikiConfig:       &types.WikiConfig{IngestBatchSize: 5},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: false},
		}},
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{
			ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusDeleting,
		}},
		chunkRepo:   &wikiQueueChunkRepoStub{},
		wikiService: &wikiQueuePageServiceStub{listErr: cleanupErr},
		pendingRepo: pending,
		task:        &wikiQueueTaskEnqueuerStub{},
	}
	payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

	if err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload)); err != nil {
		t.Fatalf("disabled-Wiki failed retract settlement error = %v", err)
	}
	if len(pending.archived) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(pending.archived))
	}
	if !strings.Contains(pending.archived[0].LastError, cleanupErr.Error()) {
		t.Fatalf("dead-letter cause = %q, want cleanup error", pending.archived[0].LastError)
	}
	if len(pending.rows) != 0 {
		t.Fatalf("dead-lettered retract left %d pending rows", len(pending.rows))
	}
}

func TestProcessWikiIngestRejectsTriggerTenantMismatchWithoutTouchingQueue(t *testing.T) {
	pending := &wikiQueuePendingRepoStub{
		rows:          []*types.TaskPendingOp{wikiPendingRow(1, WikiPendingOp{Op: WikiOpIngest, KnowledgeID: "knowledge-1"})},
		countFromRows: true,
	}
	svc := &wikiIngestService{
		kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
			ID: "kb-1", TenantID: 7, WikiConfig: &types.WikiConfig{IngestBatchSize: 5},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		}},
		pendingRepo: pending,
	}
	payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

	err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	require.ErrorContains(t, err, "trigger tenant mismatch")
	assert.Len(t, pending.rows, 1)
	assert.Empty(t, pending.deletedIDs)
	assert.Empty(t, pending.archived)
}

func TestRebuildIndexPageDoesNotApplySameDurableChangeTwice(t *testing.T) {
	indexPage := &types.WikiPage{
		ID: "index-1", KnowledgeBaseID: "kb-1", Slug: "index",
		PageType: types.WikiPageTypeIndex, Status: types.WikiPageStatusPublished,
		Content: "existing intro", Summary: "existing intro",
	}
	pages := &wikiQueuePageServiceStub{indexPage: indexPage}
	svc := &wikiIngestService{wikiService: pages}
	change := wikiIndexChange{
		SourceOpID: 91, KnowledgeID: "knowledge-1",
		Description: "<document_removed><title>old</title></document_removed>",
		Retract:     true,
	}
	model := &templateCaptureChatModel{response: "updated intro"}
	require.NoError(t, svc.rebuildIndexPage(
		context.Background(), model, WikiIngestPayload{KnowledgeBaseID: "kb-1"}, []wikiIndexChange{change}, "English",
	))
	require.Len(t, pages.contentWrites, 1)
	assert.Equal(t, "updated intro", pages.contentWrites[0].Content)

	retryModel := &templateCaptureChatModel{err: errors.New("must not be called")}
	require.NoError(t, svc.rebuildIndexPage(
		context.Background(), retryModel, WikiIngestPayload{KnowledgeBaseID: "kb-1"}, []wikiIndexChange{change}, "English",
	))
	assert.Empty(t, retryModel.prompt)
	assert.Len(t, pages.contentWrites, 1)
}

func TestRebuildIndexPageRetractLLMFailureKeepsChangeUnapplied(t *testing.T) {
	indexPage := &types.WikiPage{
		ID: "index-1", KnowledgeBaseID: "kb-1", Slug: "index",
		PageType: types.WikiPageTypeIndex, Status: types.WikiPageStatusArchived,
		Content: "intro that still mentions deleted document", Summary: "stale intro",
	}
	require.NoError(t, wikidelete.Quarantine(indexPage, "knowledge-1"))
	pages := &wikiQueuePageServiceStub{indexPage: indexPage}
	svc := &wikiIngestService{wikiService: pages}
	modelErr := errors.New("rate limited")
	change := wikiIndexChange{
		SourceOpID: 92, KnowledgeID: "knowledge-1", Retract: true,
		Description: "<document_removed><title>old</title></document_removed>",
	}

	err := svc.rebuildIndexPage(
		context.Background(),
		&templateCaptureChatModel{err: modelErr},
		WikiIngestPayload{KnowledgeBaseID: "kb-1"},
		[]wikiIndexChange{change},
		"English",
	)
	require.ErrorIs(t, err, modelErr)
	assert.Empty(t, pages.contentWrites, "failed retract intro must not be persisted or marked applied")
	applied, markerErr := wikidelete.IsApplied(indexPage, change.SourceOpID)
	require.NoError(t, markerErr)
	assert.False(t, applied)
	pendingSources, markerErr := wikidelete.PendingSources(indexPage)
	require.NoError(t, markerErr)
	assert.Equal(t, []string{"knowledge-1"}, pendingSources)
}

func TestResolveRetractSlugSetLookupFailurePropagates(t *testing.T) {
	lookupErr := errors.New("source-ref query failed")
	svc := &wikiIngestService{wikiService: &wikiQueuePageServiceStub{listErr: lookupErr}}

	slugs, err := svc.resolveRetractSlugSet(context.Background(), "kb-1", WikiPendingOp{
		Op:          WikiOpRetract,
		KnowledgeID: "knowledge-1",
		PageSlugs:   []string{"entity/snapshot"},
	})
	if !errors.Is(err, lookupErr) {
		t.Fatalf("resolveRetractSlugSet() error = %v, want wrapped lookup error", err)
	}
	if slugs != nil {
		t.Fatalf("resolveRetractSlugSet() returned partial slugs on failed authoritative lookup: %v", slugs)
	}
}

func TestGetExistingPageSlugsForKnowledgeFailurePropagates(t *testing.T) {
	lookupErr := errors.New("old-page snapshot query failed")
	svc := &wikiIngestService{wikiService: &wikiQueuePageServiceStub{listSlugsErr: lookupErr}}

	slugs, err := svc.getExistingPageSlugsForKnowledge(context.Background(), "kb-1", "knowledge-1")
	if !errors.Is(err, lookupErr) {
		t.Fatalf("getExistingPageSlugsForKnowledge() error = %v, want wrapped lookup error", err)
	}
	if slugs != nil {
		t.Fatalf("getExistingPageSlugsForKnowledge() slugs = %v, want nil on failed snapshot", slugs)
	}
}

func TestPublishDraftPagesReportsReadFailure(t *testing.T) {
	readErr := errors.New("publication read failed")
	svc := &wikiIngestService{wikiService: &wikiQueuePageServiceStub{getErr: readErr}}

	failures := svc.publishDraftPages(context.Background(), 42, "kb-1", []string{"entity/acme"}, nil)
	if len(failures) != 1 || !errors.Is(failures["entity/acme"], readErr) {
		t.Fatalf("publishDraftPages() failures = %v, want wrapped read error for entity/acme", failures)
	}
}

func TestPublishDraftPagesSettlesRetractOnlyPageAlreadyDeleted(t *testing.T) {
	pages := &wikiQueuePageServiceStub{getErr: apprepo.ErrWikiPageNotFound}
	svc := &wikiIngestService{wikiService: pages}
	updates := map[string][]SlugUpdate{
		"entity/acme": {
			{Slug: "entity/acme", Type: "retract", KnowledgeID: "knowledge-1"},
			{Slug: "entity/acme", Type: "retractStale", KnowledgeID: "knowledge-1"},
		},
	}

	failures := svc.publishDraftPages(
		context.Background(), 42, "kb-1", []string{"entity/acme"}, updates,
	)

	require.Empty(t, failures)
	require.Equal(t, 1, pages.getCalls)
}

func TestWikiUpdatesAreRetractOnlyRejectsMissingAndMixedUpdates(t *testing.T) {
	require.False(t, wikiUpdatesAreRetractOnly(nil))
	require.False(t, wikiUpdatesAreRetractOnly([]SlugUpdate{
		{Type: "retract"},
		{Type: types.WikiPageTypeEntity},
	}))
	require.True(t, wikiUpdatesAreRetractOnly([]SlugUpdate{
		{Type: "retract"},
		{Type: "retractStale"},
	}))
}

func TestProcessWikiIngestUnknownOpDeadLettersWithoutModel(t *testing.T) {
	pending := &wikiQueuePendingRepoStub{
		rows: []*types.TaskPendingOp{wikiPendingRow(1, WikiPendingOp{
			Op:          "mystery",
			KnowledgeID: "knowledge-1",
		})},
		countFromRows: true,
		incrCount:     workretry.DefaultWikiMaxAttempts,
	}
	svc := &wikiIngestService{
		kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			WikiConfig:       &types.WikiConfig{},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		}},
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{ID: "knowledge-1"}},
		pendingRepo:  pending,
		task:         &wikiQueueTaskEnqueuerStub{},
	}
	payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

	if err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload)); err != nil {
		t.Fatalf("unknown-op settlement error = %v", err)
	}
	if len(pending.archived) != 1 || !strings.Contains(pending.archived[0].LastError, "unsupported") {
		t.Fatalf("unknown-op archive = %+v, want durable unsupported-op cause", pending.archived)
	}
	if len(pending.rows) != 0 {
		t.Fatalf("unknown op left %d pending rows after dead-letter", len(pending.rows))
	}
}

func TestProcessWikiIngestMissingDerivativeModelWaitsWithoutDeadLetter(t *testing.T) {
	pending := &wikiQueuePendingRepoStub{
		rows: []*types.TaskPendingOp{wikiPendingRow(1, WikiPendingOp{
			Op:          WikiOpIngest,
			KnowledgeID: "knowledge-1",
		})},
		countFromRows: true,
		incrCount:     workretry.DefaultWikiMaxAttempts,
	}
	svc := &wikiIngestService{
		kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			WikiConfig:       &types.WikiConfig{},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		}},
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{ID: "knowledge-1"}},
		pendingRepo:  pending,
		task:         &wikiQueueTaskEnqueuerStub{},
	}
	payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

	err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	if !modeladmission.IsModelWorkDeferred(err) {
		t.Fatalf("missing derivative model error = %v, want budget-free durable wait", err)
	}
	if pending.peekCalls != 1 {
		t.Fatalf("missing-model path peek calls = %d, want 1", pending.peekCalls)
	}
	if len(pending.rows) != 1 || len(pending.incrementedIDs) != 0 || len(pending.archived) != 0 {
		t.Fatalf("waiting model mutated queue: rows=%d increments=%v archives=%d",
			len(pending.rows), pending.incrementedIDs, len(pending.archived))
	}
}

func TestProcessWikiIngestUnavailablePublishedModelWaitsWithoutDeadLetter(t *testing.T) {
	pending := &wikiQueuePendingRepoStub{
		rows: []*types.TaskPendingOp{wikiPendingRow(1, WikiPendingOp{
			Op:          WikiOpIngest,
			KnowledgeID: "knowledge-1",
		})},
		countFromRows: true,
		incrCount:     workretry.DefaultWikiMaxAttempts,
	}
	svc := &wikiIngestService{
		kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			SummaryModelID:   "deleted-model",
			WikiConfig:       &types.WikiConfig{},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		}},
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{ID: "knowledge-1"}},
		modelService: &wikiQueueModelServiceStub{err: ErrModelNotFound},
		pendingRepo:  pending,
		task:         &wikiQueueTaskEnqueuerStub{},
	}
	payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

	err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	if !modeladmission.IsModelWorkDeferred(err) || !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("unavailable published model error = %v, want deferred not-found cause", err)
	}
	if len(pending.rows) != 1 || len(pending.incrementedIDs) != 0 || len(pending.archived) != 0 {
		t.Fatalf("unavailable model mutated queue: rows=%d increments=%v archives=%d",
			len(pending.rows), pending.incrementedIDs, len(pending.archived))
	}
}

func TestProcessWikiIngestInvalidDedicatedModelWaitsForAdminRepair(t *testing.T) {
	configurationErr := fmt.Errorf("%w: unsupported chat model source", ErrChatModelConfiguration)
	pending := &wikiQueuePendingRepoStub{
		rows: []*types.TaskPendingOp{wikiPendingRow(1, WikiPendingOp{
			Op:          WikiOpIngest,
			KnowledgeID: "knowledge-1",
		})},
		countFromRows: true,
		incrCount:     workretry.DefaultWikiMaxAttempts,
	}
	svc := &wikiIngestService{
		kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			SummaryModelID:   "misconfigured-model",
			WikiConfig:       &types.WikiConfig{},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		}},
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{ID: "knowledge-1"}},
		modelService: &wikiQueueModelServiceStub{err: configurationErr},
		pendingRepo:  pending,
		task:         &wikiQueueTaskEnqueuerStub{},
	}
	payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

	err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	if !modeladmission.IsModelWorkDeferred(err) || !errors.Is(err, ErrChatModelConfiguration) {
		t.Fatalf("invalid dedicated model error = %v, want deferred configuration cause", err)
	}
	if len(pending.rows) != 1 || len(pending.incrementedIDs) != 0 || len(pending.archived) != 0 {
		t.Fatalf("invalid dedicated model mutated queue: rows=%d increments=%v archives=%d",
			len(pending.rows), pending.incrementedIDs, len(pending.archived))
	}
}

func TestProcessWikiIngestTransientModelLookupErrorKeepsPendingRows(t *testing.T) {
	modelErr := errors.New("model repository connection reset")
	pending := &wikiQueuePendingRepoStub{
		rows: []*types.TaskPendingOp{wikiPendingRow(1, WikiPendingOp{
			Op:          WikiOpIngest,
			KnowledgeID: "knowledge-1",
		})},
		countFromRows: true,
	}
	svc := &wikiIngestService{
		kbService: &wikiQueueKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			SummaryModelID:   "model-1",
			WikiConfig:       &types.WikiConfig{},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		}},
		knowledgeSvc: &wikiQueueKnowledgeServiceStub{knowledge: &types.Knowledge{ID: "knowledge-1"}},
		modelService: &wikiQueueModelServiceStub{err: modelErr},
		pendingRepo:  pending,
		task:         &wikiQueueTaskEnqueuerStub{},
	}
	payload, _ := json.Marshal(WikiIngestPayload{TenantID: 42, KnowledgeBaseID: "kb-1"})

	err := svc.ProcessWikiIngest(context.Background(), asynq.NewTask(types.TypeWikiIngest, payload))
	if !errors.Is(err, modelErr) {
		t.Fatalf("ProcessWikiIngest() error = %v, want transient model lookup error", err)
	}
	if pending.peekCalls != 1 || pending.deleteCalls != 0 || len(pending.incrementedIDs) != 0 || len(pending.archived) != 0 {
		t.Fatalf("transient model lookup mutated queue: peek=%d delete=%d increments=%v archives=%d",
			pending.peekCalls, pending.deleteCalls, pending.incrementedIDs, len(pending.archived))
	}
	if len(pending.rows) != 1 {
		t.Fatalf("transient model lookup left %d rows, want 1", len(pending.rows))
	}
}
