package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentoutcome"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/custom/modules/taskretry"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

type imageGenerationKnowledgeRepoStub struct {
	interfaces.KnowledgeRepository
	knowledge      *types.Knowledge
	calls          int
	completed      map[string]struct{}
	outcomes       map[string]string
	rejectCanceled bool
}

type imageGenerationKBServiceStub struct {
	interfaces.KnowledgeBaseService
	kb  *types.KnowledgeBase
	err error
}

func (s *imageGenerationKBServiceStub) GetKnowledgeBaseByIDOnly(
	context.Context,
	string,
) (*types.KnowledgeBase, error) {
	return s.kb, s.err
}

func (s *imageGenerationKnowledgeRepoStub) RecordKnowledgeFanoutCompletion(
	ctx context.Context, _ uint64, _, _, _, item string,
) (bool, error) {
	if s.rejectCanceled && ctx.Err() != nil {
		return false, ctx.Err()
	}
	if s.completed == nil {
		s.completed = make(map[string]struct{})
	}
	_, exists := s.completed[item]
	s.completed[item] = struct{}{}
	return !exists, nil
}

func (s *imageGenerationKnowledgeRepoStub) ListKnowledgeFanoutCompletions(
	ctx context.Context, _ uint64, _, _, _ string,
) ([]string, error) {
	if s.rejectCanceled && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	items := make([]string, 0, len(s.completed))
	for item := range s.completed {
		items = append(items, item)
	}
	return items, nil
}

func (s *imageGenerationKnowledgeRepoStub) CountKnowledgeFanoutCompletions(
	ctx context.Context, _ uint64, _, _, _ string,
) (int64, error) {
	if s.rejectCanceled && ctx.Err() != nil {
		return 0, ctx.Err()
	}
	var count int64
	for item := range s.completed {
		if len(item) < len("enrichment:") || item[:len("enrichment:")] != "enrichment:" {
			count++
		}
	}
	return count, nil
}

func (s *imageGenerationKnowledgeRepoStub) KnowledgeFanoutCompletionExists(
	ctx context.Context, _ uint64, _, _, _, item string,
) (bool, error) {
	if s.rejectCanceled && ctx.Err() != nil {
		return false, ctx.Err()
	}
	_, exists := s.completed[item]
	return exists, nil
}

func (s *imageGenerationKnowledgeRepoStub) RecordGenerationOutcome(
	ctx context.Context,
	_ uint64,
	_, _, _ string,
	item string,
	status string,
	_ string,
) (bool, error) {
	if s.rejectCanceled && ctx.Err() != nil {
		return false, ctx.Err()
	}
	if s.outcomes == nil {
		s.outcomes = make(map[string]string)
	}
	if _, exists := s.outcomes[item]; exists {
		return false, nil
	}
	s.outcomes[item] = status
	return true, nil
}

func (s *imageGenerationKnowledgeRepoStub) GetGenerationOutcomeAggregate(
	context.Context, uint64, string, string, string,
) (enrichmentoutcome.Aggregate, error) {
	var aggregate enrichmentoutcome.Aggregate
	for _, status := range s.outcomes {
		aggregate.Total++
		switch status {
		case enrichmentoutcome.StatusFailed:
			aggregate.Failed++
		case enrichmentoutcome.StatusDegraded:
			aggregate.Degraded++
		case enrichmentoutcome.StatusCompleted:
			aggregate.Completed++
		}
	}
	return aggregate, nil
}

func (s *imageGenerationKnowledgeRepoStub) GetKnowledgeByID(context.Context, uint64, string) (*types.Knowledge, error) {
	s.calls++
	return s.knowledge, nil
}

func TestImageMultimodalStaleGenerationRejectsBeforeRedisOrVLM(t *testing.T) {
	repo := &imageGenerationKnowledgeRepoStub{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusProcessing,
		ProcessingGeneration: "new-generation",
	}}
	svc := &ImageMultimodalService{knowledgeRepo: repo}
	payload, _ := json.Marshal(types.ImageMultimodalPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "old-generation",
		ChunkID:              "chunk-1",
		ImageURL:             "local://image",
	})
	if err := svc.Handle(context.Background(), asynq.NewTask(types.TypeImageMultimodal, payload)); err != nil {
		t.Fatalf("Handle() stale generation error = %v", err)
	}
	if repo.calls != 1 {
		t.Fatalf("knowledge reads = %d, want 1", repo.calls)
	}
}

func TestImageMultimodalMissingGenerationFailsClosed(t *testing.T) {
	svc := &ImageMultimodalService{}
	payload, _ := json.Marshal(types.ImageMultimodalPayload{
		TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1", ChunkID: "chunk-1",
	})
	err := svc.Handle(context.Background(), asynq.NewTask(types.TypeImageMultimodal, payload))
	if err == nil {
		t.Fatal("Handle() error = nil, want missing generation rejection")
	}
}

func TestResolveImageVLMDisabledIsPermanentConfigurationFailure(t *testing.T) {
	t.Parallel()

	svc := &ImageMultimodalService{
		kbService: &imageGenerationKBServiceStub{
			kb: &types.KnowledgeBase{
				ID:        "kb-1",
				VLMConfig: types.VLMConfig{Enabled: false},
			},
		},
	}
	_, _, err := svc.resolveVLM(context.Background(), "kb-1", "")
	if !errors.Is(err, errImageMultimodalVLMNotConfigured) {
		t.Fatalf("resolveVLM() error = %v, want permanent VLM configuration failure", err)
	}
}

func TestResolveImageVLMMissingKnowledgeBaseIsPermanentConfigurationFailure(t *testing.T) {
	t.Parallel()

	svc := &ImageMultimodalService{
		kbService: &imageGenerationKBServiceStub{},
	}
	_, _, err := svc.resolveVLM(context.Background(), "missing-kb", "")
	if !errors.Is(err, errImageMultimodalVLMNotConfigured) {
		t.Fatalf("resolveVLM() error = %v, want permanent VLM configuration failure", err)
	}
}

func TestStableImageChunkIDIsGenerationScoped(t *testing.T) {
	payload := types.ImageMultimodalPayload{
		TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ImageIndex: 3,
	}
	first := stableImageChunkID(payload, "ocr")
	if again := stableImageChunkID(payload, "ocr"); again != first {
		t.Fatalf("same image artifact ID changed: %s != %s", again, first)
	}
	if caption := stableImageChunkID(payload, "caption"); caption == first {
		t.Fatal("OCR and caption artifact IDs collided")
	}
	payload.ProcessingGeneration = "generation-2"
	if nextGeneration := stableImageChunkID(payload, "ocr"); nextGeneration == first {
		t.Fatal("different generations reused an image artifact ID")
	}
}

func TestMultimodalChunkIndexInfoPreservesRetrievalFlags(t *testing.T) {
	chunk := &types.Chunk{
		ID:              "image-chunk",
		KnowledgeID:     "knowledge",
		KnowledgeBaseID: "knowledge-base",
		Content:         "recognized image text",
		IsEnabled:       true,
		Flags:           types.ChunkFlagRecommended,
	}

	index := multimodalChunkIndexInfo(chunk)
	if index.SourceID != chunk.ID || index.ChunkID != chunk.ID {
		t.Fatalf("index identity = source %q chunk %q, want %q", index.SourceID, index.ChunkID, chunk.ID)
	}
	if !index.IsEnabled {
		t.Fatal("multimodal index unexpectedly disabled")
	}
	if !index.IsRecommended {
		t.Fatal("multimodal index lost recommended flag")
	}
}

func TestTerminalImageFailureOutcomeSurvivesCancelledWorkerContext(t *testing.T) {
	repo := &imageGenerationKnowledgeRepoStub{rejectCanceled: true}
	svc := &ImageMultimodalService{knowledgeRepo: repo}
	payload := types.ImageMultimodalPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "generation-1",
		ImageIndex:           3,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.recordTerminalImageFailure(ctx, payload, errors.New("provider failed")); err != nil {
		t.Fatalf("recordTerminalImageFailure() error = %v", err)
	}
	if status := repo.outcomes["multimodal.image[3]"]; status != enrichmentoutcome.StatusFailed {
		t.Fatalf("terminal outcome = %q, want failed", status)
	}
}

func serviceFanoutTestRedis(t *testing.T) *redis.Client {
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

func TestImageFanInPostProcessEnqueueFailureReplaysWithoutDoubleCompletion(t *testing.T) {
	ctx := context.Background()
	client := serviceFanoutTestRedis(t)
	suffix := time.Now().UnixNano()
	payload := types.ImageMultimodalPayload{
		TenantID:             42,
		KnowledgeID:          fmt.Sprintf("image-fanin-%d", suffix),
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: fmt.Sprintf("generation-%d", suffix),
		ChunkID:              "chunk-1",
		ImageURL:             "local://image",
		Attempt:              7,
		ImageIndex:           0,
	}
	plan := processownership.FanoutPlan{
		Version:              processownership.FanoutPlanVersion,
		TenantID:             payload.TenantID,
		KnowledgeID:          payload.KnowledgeID,
		KnowledgeBaseID:      payload.KnowledgeBaseID,
		ProcessingGeneration: payload.ProcessingGeneration,
		Attempt:              payload.Attempt,
		Images: []processownership.ImageFanout{{
			ChunkID: payload.ChunkID, ImageURL: payload.ImageURL, Index: payload.ImageIndex,
		}},
	}
	completionStore := &imageGenerationKnowledgeRepoStub{}
	t.Cleanup(func() {
		_ = processownership.ClearFanIn(ctx, client, payload.KnowledgeID, payload.ProcessingGeneration)
	})
	enqueueErr := errors.New("redis queue unavailable")
	failing := &wikiQueueTaskEnqueuerStub{err: enqueueErr}
	svc := &ImageMultimodalService{redisClient: client, taskEnqueuer: failing}
	if err := svc.checkAndFinalizeAllImages(ctx, payload, plan, completionStore); !errors.Is(err, enqueueErr) {
		t.Fatalf("first completion error = %v, want enqueue error", err)
	}
	remaining, err := processownership.DurableFanoutRemaining(ctx, completionStore, client, plan)
	if err != nil || remaining != 0 {
		t.Fatalf("remaining after failed enqueue = %d err=%v, want 0", remaining, err)
	}

	success := &wikiQueueTaskEnqueuerStub{}
	svc.taskEnqueuer = success
	if err := svc.checkAndFinalizeAllImages(ctx, payload, plan, completionStore); err != nil {
		t.Fatalf("completion replay error = %v", err)
	}
	if len(success.tasks) != 1 || success.tasks[0].Type() != types.TypeKnowledgePostProcess {
		t.Fatalf("replayed tasks = %d, want one postprocess", len(success.tasks))
	}
	var postPayload types.KnowledgePostProcessPayload
	if err := json.Unmarshal(success.tasks[0].Payload(), &postPayload); err != nil {
		t.Fatal(err)
	}
	if postPayload.ProcessingGeneration != payload.ProcessingGeneration || postPayload.Attempt != payload.Attempt {
		t.Fatalf("postprocess identity = generation:%q attempt:%d", postPayload.ProcessingGeneration, postPayload.Attempt)
	}
	var taskID string
	for _, opt := range success.opts[0] {
		if opt.Type() == asynq.TaskIDOpt {
			taskID, _ = opt.Value().(string)
		}
	}
	wantTaskID := processownership.PostProcessTaskID(payload.KnowledgeID, payload.ProcessingGeneration)
	if taskID != wantTaskID {
		t.Fatalf("postprocess TaskID = %q, want %q", taskID, wantTaskID)
	}
}

func TestImageTerminalFanInSurvivesCancelledWorkerContext(t *testing.T) {
	payload := types.ImageMultimodalPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-cancelled-image",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "generation-1",
		ChunkID:              "chunk-1",
		ImageURL:             "local://image",
		ImageIndex:           0,
	}
	plan := processownership.FanoutPlan{
		Version:              processownership.FanoutPlanVersion,
		TenantID:             payload.TenantID,
		KnowledgeID:          payload.KnowledgeID,
		KnowledgeBaseID:      payload.KnowledgeBaseID,
		ProcessingGeneration: payload.ProcessingGeneration,
		Images: []processownership.ImageFanout{{
			ChunkID: payload.ChunkID, ImageURL: payload.ImageURL, Index: 0,
		}},
	}
	store := &imageGenerationKnowledgeRepoStub{rejectCanceled: true}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &ImageMultimodalService{taskEnqueuer: enqueuer}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.checkAndFinalizeAllImages(ctx, payload, plan, store); err != nil {
		t.Fatalf("terminal fan-in on cancelled worker context: %v", err)
	}
	if _, completed := store.completed[processownership.ImageFanoutItem(0)]; !completed {
		t.Fatal("cancelled worker context lost durable image completion")
	}
	if len(enqueuer.tasks) != 1 || enqueuer.tasks[0].Type() != types.TypeKnowledgePostProcess {
		t.Fatalf("postprocess tasks = %d, want 1", len(enqueuer.tasks))
	}
}

func TestImageFinalAttemptHonorsExpandedRetryBudgetAcrossRollout(t *testing.T) {
	t.Setenv("CUSTOM_WORK_RETRY_IMAGE_MAX_ATTEMPTS", "10")

	oldDelivery := taskretry.WithMetadata(context.Background(), 3, 3)
	if isFinalAsynqAttempt(oldDelivery) {
		t.Fatal("old MaxRetry delivery must remain incomplete for stable-task replay")
	}
	notYetFinal := taskretry.WithMetadata(context.Background(), 8, 9)
	if isFinalAsynqAttempt(notYetFinal) {
		t.Fatal("ninth total attempt must not terminally complete a ten-attempt budget")
	}
	final := taskretry.WithMetadata(context.Background(), 9, 9)
	if !isFinalAsynqAttempt(final) {
		t.Fatal("tenth total attempt must terminally complete the bounded image item")
	}
}
