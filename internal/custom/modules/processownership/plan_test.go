package processownership

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

type fanoutTestEnqueuer struct {
	calls  int
	failAt int
	err    error
}

type countingCompletionStore struct {
	items       map[string]struct{}
	recordCalls int
	countCalls  int
	listCalls   int
	existsCalls int
}

func (s *countingCompletionStore) RecordKnowledgeFanoutCompletion(
	_ context.Context, _ uint64, _, _, _, item string,
) (bool, error) {
	s.recordCalls++
	if s.items == nil {
		s.items = make(map[string]struct{})
	}
	_, exists := s.items[item]
	s.items[item] = struct{}{}
	return !exists, nil
}

func (s *countingCompletionStore) ListKnowledgeFanoutCompletions(
	context.Context, uint64, string, string, string,
) ([]string, error) {
	s.listCalls++
	items := make([]string, 0, len(s.items))
	for item := range s.items {
		items = append(items, item)
	}
	return items, nil
}

func (s *countingCompletionStore) CountKnowledgeFanoutCompletions(
	context.Context, uint64, string, string, string,
) (int64, error) {
	s.countCalls++
	return int64(len(s.items)), nil
}

func (s *countingCompletionStore) KnowledgeFanoutCompletionExists(
	_ context.Context, _ uint64, _, _, _, item string,
) (bool, error) {
	s.existsCalls++
	_, exists := s.items[item]
	return exists, nil
}

func (e *fanoutTestEnqueuer) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	e.calls++
	if e.failAt > 0 && e.calls == e.failAt {
		return nil, e.err
	}
	return &asynq.TaskInfo{ID: fmt.Sprintf("task-%d", e.calls), Type: task.Type(), Queue: types.QueueDefault}, nil
}

func fanoutTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR is not configured")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		client.Close()
		t.Skipf("Redis is unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func uniqueFanoutPlan() FanoutPlan {
	suffix := time.Now().UnixNano()
	return FanoutPlan{
		Version:              FanoutPlanVersion,
		TenantID:             42,
		KnowledgeID:          fmt.Sprintf("fanout-test-%d", suffix),
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: fmt.Sprintf("generation-%d", suffix),
		Images: []ImageFanout{
			{ChunkID: "chunk-1", ImageURL: "local://image-1", Index: 0},
			{ChunkID: "chunk-2", ImageURL: "local://image-2", Index: 1},
		},
	}
}

func cleanupFanoutPlan(t *testing.T, client *redis.Client, plan FanoutPlan) {
	t.Helper()
	t.Cleanup(func() {
		_ = ClearFanIn(context.Background(), client, plan.KnowledgeID, plan.ProcessingGeneration)
	})
}

func TestDispatchFanoutReplayDoesNotResetPartialCompletion(t *testing.T) {
	ctx := context.Background()
	client := fanoutTestRedis(t)
	plan := uniqueFanoutPlan()
	cleanupFanoutPlan(t, client, plan)

	enqueueErr := errors.New("second image enqueue failed")
	first := &fanoutTestEnqueuer{failAt: 2, err: enqueueErr}
	if err := DispatchFanout(ctx, first, client, plan); !errors.Is(err, enqueueErr) {
		t.Fatalf("DispatchFanout() error = %v, want partial enqueue error", err)
	}
	remaining, added, err := CompleteFanoutItem(
		ctx, client, plan.KnowledgeID, plan.ProcessingGeneration, ImageFanoutItem(0),
	)
	if err != nil || !added || remaining != 1 {
		t.Fatalf("first completion = remaining:%d added:%v err:%v, want 1/true/nil", remaining, added, err)
	}

	if err := DispatchFanout(ctx, &fanoutTestEnqueuer{}, client, plan); err != nil {
		t.Fatalf("DispatchFanout() replay error = %v", err)
	}
	remaining, err = FanoutRemaining(ctx, client, plan.KnowledgeID, plan.ProcessingGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining after replay = %d, want 1 (counter must not reset)", remaining)
	}
}

func TestCompleteFanoutItemIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := fanoutTestRedis(t)
	plan := uniqueFanoutPlan()
	cleanupFanoutPlan(t, client, plan)
	if err := DispatchFanout(ctx, &fanoutTestEnqueuer{}, client, plan); err != nil {
		t.Fatal(err)
	}

	remaining, added, err := CompleteFanoutItem(
		ctx, client, plan.KnowledgeID, plan.ProcessingGeneration, ImageFanoutItem(0),
	)
	if err != nil || !added || remaining != 1 {
		t.Fatalf("first completion = remaining:%d added:%v err:%v", remaining, added, err)
	}
	remaining, added, err = CompleteFanoutItem(
		ctx, client, plan.KnowledgeID, plan.ProcessingGeneration, ImageFanoutItem(0),
	)
	if err != nil || added || remaining != 1 {
		t.Fatalf("duplicate completion = remaining:%d added:%v err:%v, want 1/false/nil", remaining, added, err)
	}
}

func TestCompleteFanoutItemFailsWhenDurableTotalMissing(t *testing.T) {
	client := fanoutTestRedis(t)
	plan := uniqueFanoutPlan()
	cleanupFanoutPlan(t, client, plan)
	_, _, err := CompleteFanoutItem(
		context.Background(), client, plan.KnowledgeID, plan.ProcessingGeneration, ImageFanoutItem(0),
	)
	if err == nil {
		t.Fatal("CompleteFanoutItem() error = nil, want missing durable total error")
	}
}

func TestCompleteDurableFanoutLargePlanDoesNotListPerItem(t *testing.T) {
	const itemCount = 1000
	plan := FanoutPlan{
		Version:              FanoutPlanVersion,
		TenantID:             42,
		KnowledgeID:          "knowledge-large",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "generation-large",
		Images:               make([]ImageFanout, 0, itemCount),
	}
	for i := 0; i < itemCount; i++ {
		plan.Images = append(plan.Images, ImageFanout{
			ChunkID: fmt.Sprintf("chunk-%d", i), ImageURL: fmt.Sprintf("local://image-%d", i), Index: i,
		})
	}
	store := &countingCompletionStore{}
	for i := 0; i < itemCount; i++ {
		remaining, inserted, err := CompleteDurableFanoutItem(
			context.Background(), store, nil, plan, ImageFanoutItem(i),
		)
		if err != nil || !inserted || remaining != int64(itemCount-i-1) {
			t.Fatalf("completion %d = remaining:%d inserted:%v err:%v", i, remaining, inserted, err)
		}
	}
	if store.recordCalls != itemCount || store.countCalls != itemCount {
		t.Fatalf("scalar DB calls = record:%d count:%d, want %d each",
			store.recordCalls, store.countCalls, itemCount)
	}
	if store.listCalls != 0 || store.existsCalls != 0 {
		t.Fatalf("large-plan completion transferred full lists: list=%d exists=%d",
			store.listCalls, store.existsCalls)
	}
}

func TestDispatchFanoutSkipsDurablyCompletedItemsAfterRedisRetention(t *testing.T) {
	plan := uniqueFanoutPlan()
	store := &countingCompletionStore{
		items: map[string]struct{}{
			ImageFanoutItem(0): {},
		},
	}
	enqueuer := &fanoutTestEnqueuer{}

	if err := DispatchFanout(
		context.Background(), enqueuer, nil, plan, store,
	); err != nil {
		t.Fatalf("DispatchFanout() error = %v", err)
	}
	if enqueuer.calls != 1 {
		t.Fatalf("enqueued tasks = %d, want only the one incomplete image", enqueuer.calls)
	}
	if store.listCalls != 1 {
		t.Fatalf("completion ledger list calls = %d, want 1", store.listCalls)
	}
}

func TestDurableFanoutListsOnlyWhenRedisMirrorIsMissing(t *testing.T) {
	ctx := context.Background()
	client := fanoutTestRedis(t)
	plan := uniqueFanoutPlan()
	cleanupFanoutPlan(t, client, plan)
	store := &countingCompletionStore{}

	if _, _, err := CompleteDurableFanoutItem(
		ctx, store, client, plan, ImageFanoutItem(0),
	); err != nil {
		t.Fatal(err)
	}
	if store.listCalls != 1 {
		t.Fatalf("first completion list calls = %d, want one cache rebuild", store.listCalls)
	}
	if _, _, err := CompleteDurableFanoutItem(
		ctx, store, client, plan, ImageFanoutItem(1),
	); err != nil {
		t.Fatal(err)
	}
	if store.listCalls != 1 {
		t.Fatalf("warm Redis mirror list calls = %d, want still 1", store.listCalls)
	}
}
