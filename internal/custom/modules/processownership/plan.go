package processownership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const FanoutPlanVersion = 1

// PostProcessCompletionItem is the durable generation receipt written only
// after the post-process orchestrator has published every child task and Wiki
// intent. It lives in knowledge_fanout_completions under a reserved namespace
// so a retained completed Asynq task can be distinguished from a crash that
// happened part-way through publication.
const PostProcessCompletionItem = "orchestration:postprocess"

// GenerationTaskRetention keeps successful generation-scoped tasks visible to
// Asynq long enough for a replay of the durable fan-out plan to observe the
// same TaskID as a conflict. Without retention a completed task disappears
// immediately and a recovery replay can run it twice and drain another
// enrichment slot from the same generation.
const (
	GenerationTaskRetention = 7 * 24 * time.Hour
	// EnrichmentTaskTimeout includes distributed model-admission wait time.
	// The task is durable and generation-fenced, so a short generic worker
	// timeout only converts healthy provider saturation into retry churn.
	GenerationTaskTimeout = 2 * time.Hour
	FanInTTL              = 7 * 24 * time.Hour
)

// ImageFanout contains everything required to recreate one multimodal task
// after the core document transaction has committed.
type ImageFanout struct {
	ChunkID         string `json:"chunk_id"`
	ImageURL        string `json:"image_url"`
	ImageSourceType string `json:"image_source_type,omitempty"`
	Index           int    `json:"index"`
}

type DataTableFanout struct {
	SummaryModel   string `json:"summary_model"`
	EmbeddingModel string `json:"embedding_model"`
}

type DurableFanoutCompletionStore interface {
	RecordKnowledgeFanoutCompletion(
		context.Context, uint64, string, string, string, string,
	) (bool, error)
	ListKnowledgeFanoutCompletions(
		context.Context, uint64, string, string, string,
	) ([]string, error)
	CountKnowledgeFanoutCompletions(
		context.Context, uint64, string, string, string,
	) (int64, error)
	KnowledgeFanoutCompletionExists(
		context.Context, uint64, string, string, string, string,
	) (bool, error)
}

// FanoutPlan is persisted on the knowledge row by the same atomic update that
// adopts chunks/index storage. Redis is only a fan-in accelerator; this plan is
// the durable source from which a crashed worker can rebuild every task.
type FanoutPlan struct {
	Version              int                  `json:"version"`
	TenantID             uint64               `json:"tenant_id"`
	KnowledgeID          string               `json:"knowledge_id"`
	KnowledgeBaseID      string               `json:"knowledge_base_id"`
	ProcessingGeneration string               `json:"processing_generation"`
	Language             string               `json:"language,omitempty"`
	Attempt              int                  `json:"attempt,omitempty"`
	Tracing              types.TracingContext `json:"tracing,omitempty"`
	Images               []ImageFanout        `json:"images,omitempty"`
	DataTable            *DataTableFanout     `json:"data_table,omitempty"`
}

func (p FanoutPlan) Validate() error {
	if p.Version != FanoutPlanVersion || p.TenantID == 0 ||
		strings.TrimSpace(p.KnowledgeID) == "" ||
		strings.TrimSpace(p.KnowledgeBaseID) == "" ||
		strings.TrimSpace(p.ProcessingGeneration) == "" {
		return fmt.Errorf("incomplete document fanout plan")
	}
	for position, image := range p.Images {
		if strings.TrimSpace(image.ChunkID) == "" || strings.TrimSpace(image.ImageURL) == "" || image.Index != position {
			return fmt.Errorf("invalid image fanout entry %d", image.Index)
		}
	}
	if p.DataTable != nil && (strings.TrimSpace(p.DataTable.SummaryModel) == "" ||
		strings.TrimSpace(p.DataTable.EmbeddingModel) == "") {
		return fmt.Errorf("invalid data-table fanout configuration")
	}
	return nil
}

func MarshalFanoutPlan(plan FanoutPlan) ([]byte, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(plan)
}

func ParseFanoutPlan(raw []byte) (FanoutPlan, error) {
	var plan FanoutPlan
	if len(raw) == 0 {
		return plan, fmt.Errorf("document fanout plan is empty")
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return plan, fmt.Errorf("decode document fanout plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return plan, err
	}
	return plan, nil
}

func DocumentOwner(knowledgeID, generation string) string {
	return "document:" + knowledgeID + ":" + generation
}

func DocumentTaskID(knowledgeID, generation, queue string) string {
	return "document:" + queue + ":" + knowledgeID + ":" + generation
}

func ManualTaskID(knowledgeID, generation string) string {
	return "manual:" + knowledgeID + ":" + generation
}

func PostProcessTaskID(knowledgeID, generation string) string {
	return "postprocess:" + knowledgeID + ":" + generation
}

func ImageTaskID(knowledgeID, generation string, index int) string {
	return fmt.Sprintf("image:%s:%s:%d", knowledgeID, generation, index)
}

func DataTableSummaryTaskID(knowledgeID, generation string) string {
	return "datatable-summary:" + knowledgeID + ":" + generation
}

func SummaryTaskID(knowledgeID, generation string) string {
	return "summary:" + knowledgeID + ":" + generation
}

func QuestionTaskID(knowledgeID, generation string, batchIndex int) string {
	return fmt.Sprintf("question:%s:%s:%d", knowledgeID, generation, batchIndex)
}

func ExtractTaskID(knowledgeID, generation string, chunkIndex int) string {
	return fmt.Sprintf("extract:%s:%s:%d", knowledgeID, generation, chunkIndex)
}

func MultimodalPendingKey(knowledgeID, generation string) string {
	return "multimodal:pending:" + knowledgeID + ":" + generation
}

func FanoutDoneKey(knowledgeID, generation string) string {
	return "multimodal:done:" + knowledgeID + ":" + generation
}

func ImageFanoutItem(index int) string {
	return fmt.Sprintf("image:%d", index)
}

func DataTableFanoutItem() string {
	return "datatable"
}

func (p FanoutPlan) itemCount() int {
	count := len(p.Images)
	if p.DataTable != nil {
		count++
	}
	return count
}

func (p FanoutPlan) itemIDs() []string {
	items := make([]string, 0, p.itemCount())
	if p.DataTable != nil {
		items = append(items, DataTableFanoutItem())
	}
	for _, image := range p.Images {
		items = append(items, ImageFanoutItem(image.Index))
	}
	return items
}

func (p FanoutPlan) containsItem(item string) bool {
	if item == DataTableFanoutItem() {
		return p.DataTable != nil
	}
	const imagePrefix = "image:"
	if !strings.HasPrefix(item, imagePrefix) {
		return false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(item, imagePrefix))
	return err == nil && index >= 0 && index < len(p.Images) && p.Images[index].Index == index
}

func durableFanoutRemaining(
	ctx context.Context,
	store DurableFanoutCompletionStore,
	plan FanoutPlan,
) (int64, error) {
	if store == nil {
		return 0, errors.New("durable fanout completion store is unavailable")
	}
	completed, err := store.CountKnowledgeFanoutCompletions(
		ctx,
		plan.TenantID,
		plan.KnowledgeID,
		plan.KnowledgeBaseID,
		plan.ProcessingGeneration,
	)
	if err != nil {
		return 0, err
	}
	remaining := int64(plan.itemCount()) - completed
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

func validCompletedFanoutItems(plan FanoutPlan, completed []string) []string {
	planned := make(map[string]struct{}, plan.itemCount())
	for _, item := range plan.itemIDs() {
		planned[item] = struct{}{}
	}
	seen := make(map[string]struct{}, len(completed))
	valid := make([]string, 0, len(completed))
	for _, item := range completed {
		if _, ok := planned[item]; !ok {
			continue
		}
		if _, duplicate := seen[item]; duplicate {
			continue
		}
		seen[item] = struct{}{}
		valid = append(valid, item)
	}
	return valid
}

func mirrorFanoutCache(
	ctx context.Context,
	redisClient *redis.Client,
	plan FanoutPlan,
	completed []string,
) {
	if redisClient == nil {
		return
	}
	totalKey := MultimodalPendingKey(plan.KnowledgeID, plan.ProcessingGeneration)
	doneKey := FanoutDoneKey(plan.KnowledgeID, plan.ProcessingGeneration)
	pipe := redisClient.TxPipeline()
	pipe.Set(ctx, totalKey, plan.itemCount(), FanInTTL)
	pipe.Del(ctx, doneKey)
	if len(completed) > 0 {
		members := make([]interface{}, len(completed))
		for i, item := range completed {
			members[i] = item
		}
		pipe.SAdd(ctx, doneKey, members...)
		pipe.Expire(ctx, doneKey, FanInTTL)
	}
	_, _ = pipe.Exec(ctx)
}

// updateFanoutCache keeps the common completion path O(1): when the Redis
// generation key exists, only the current item is SADDed. A missing/expired
// key triggers one full ledger read to rebuild the mirror; lifecycle decisions
// continue to use the database COUNT regardless of cache health.
func updateFanoutCache(
	ctx context.Context,
	store DurableFanoutCompletionStore,
	redisClient *redis.Client,
	plan FanoutPlan,
	completedItem string,
	itemCompleted bool,
) {
	if redisClient == nil {
		return
	}
	totalKey := MultimodalPendingKey(plan.KnowledgeID, plan.ProcessingGeneration)
	doneKey := FanoutDoneKey(plan.KnowledgeID, plan.ProcessingGeneration)
	exists, err := redisClient.Exists(ctx, totalKey).Result()
	if err == nil && exists > 0 {
		pipe := redisClient.TxPipeline()
		pipe.Expire(ctx, totalKey, FanInTTL)
		if itemCompleted && completedItem != "" {
			pipe.SAdd(ctx, doneKey, completedItem)
			pipe.Expire(ctx, doneKey, FanInTTL)
		}
		_, _ = pipe.Exec(ctx)
		return
	}
	completed, listErr := store.ListKnowledgeFanoutCompletions(
		ctx,
		plan.TenantID,
		plan.KnowledgeID,
		plan.KnowledgeBaseID,
		plan.ProcessingGeneration,
	)
	if listErr != nil {
		return
	}
	mirrorFanoutCache(ctx, redisClient, plan, validCompletedFanoutItems(plan, completed))
}

// DurableFanoutRemaining derives fan-in progress from the database ledger and
// repairs the Redis mirror opportunistically. Redis loss never changes the
// answer returned to lifecycle code.
func DurableFanoutRemaining(
	ctx context.Context,
	store DurableFanoutCompletionStore,
	redisClient *redis.Client,
	plan FanoutPlan,
) (int64, error) {
	remaining, err := durableFanoutRemaining(ctx, store, plan)
	if err != nil {
		return 0, err
	}
	updateFanoutCache(ctx, store, redisClient, plan, "", false)
	return remaining, nil
}

func DurableFanoutItemCompleted(
	ctx context.Context,
	store DurableFanoutCompletionStore,
	redisClient *redis.Client,
	plan FanoutPlan,
	item string,
) (bool, int64, error) {
	if !plan.containsItem(item) {
		return false, 0, fmt.Errorf("fanout item %q is absent from durable plan", item)
	}
	done, err := store.KnowledgeFanoutCompletionExists(
		ctx,
		plan.TenantID,
		plan.KnowledgeID,
		plan.KnowledgeBaseID,
		plan.ProcessingGeneration,
		item,
	)
	if err != nil {
		return false, 0, err
	}
	remaining, err := durableFanoutRemaining(ctx, store, plan)
	if err != nil {
		return false, 0, err
	}
	updateFanoutCache(ctx, store, redisClient, plan, item, done)
	return done, remaining, nil
}

// CompleteDurableFanoutItem records completion in the database before
// deriving remaining work. A crash at any later point is recoverable by a
// retry reading the same ledger; Redis is only a reconstructed mirror.
func CompleteDurableFanoutItem(
	ctx context.Context,
	store DurableFanoutCompletionStore,
	redisClient *redis.Client,
	plan FanoutPlan,
	item string,
) (remaining int64, newlyCompleted bool, err error) {
	if store == nil {
		return 0, false, errors.New("durable fanout completion store is unavailable")
	}
	if !plan.containsItem(item) {
		return 0, false, fmt.Errorf("fanout item %q is absent from durable plan", item)
	}
	newlyCompleted, err = store.RecordKnowledgeFanoutCompletion(
		ctx,
		plan.TenantID,
		plan.KnowledgeID,
		plan.KnowledgeBaseID,
		plan.ProcessingGeneration,
		item,
	)
	if err != nil {
		return 0, false, err
	}
	remaining, err = durableFanoutRemaining(ctx, store, plan)
	if err != nil {
		return 0, newlyCompleted, err
	}
	updateFanoutCache(ctx, store, redisClient, plan, item, true)
	return remaining, newlyCompleted, nil
}

var completeFanoutItemScript = redis.NewScript(`
local total = redis.call('GET', KEYS[1])
if not total then
  return {-1, 0}
end
local added = redis.call('SADD', KEYS[2], ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
redis.call('PEXPIRE', KEYS[2], ARGV[2])
local done = redis.call('SCARD', KEYS[2])
return {tonumber(total) - done, added}
`)

// CompleteFanoutItem records one stable item in a Redis set and derives the
// remaining count from SCARD. SADD makes the completion idempotent even when a
// worker crashes after recording completion but before enqueueing postprocess.
func CompleteFanoutItem(
	ctx context.Context,
	redisClient *redis.Client,
	knowledgeID, generation, item string,
) (remaining int64, newlyCompleted bool, err error) {
	if redisClient == nil || knowledgeID == "" || generation == "" || item == "" {
		return 0, false, errors.New("complete fanout item: complete identity and redis are required")
	}
	values, err := completeFanoutItemScript.Run(
		ctx,
		redisClient,
		[]string{MultimodalPendingKey(knowledgeID, generation), FanoutDoneKey(knowledgeID, generation)},
		item,
		FanInTTL.Milliseconds(),
	).Slice()
	if err != nil {
		return 0, false, err
	}
	if len(values) != 2 {
		return 0, false, fmt.Errorf("complete fanout item: unexpected redis result")
	}
	remaining, ok := values[0].(int64)
	if !ok {
		return 0, false, fmt.Errorf("complete fanout item: invalid remaining value %T", values[0])
	}
	added, ok := values[1].(int64)
	if !ok {
		return 0, false, fmt.Errorf("complete fanout item: invalid added value %T", values[1])
	}
	if remaining < 0 {
		return 0, false, errors.New("complete fanout item: durable fan-in counter is missing")
	}
	return remaining, added == 1, nil
}

func FanoutRemaining(
	ctx context.Context,
	redisClient *redis.Client,
	knowledgeID, generation string,
) (int64, error) {
	if redisClient == nil {
		return 0, errors.New("fanout remaining: redis is required")
	}
	total, err := redisClient.Get(ctx, MultimodalPendingKey(knowledgeID, generation)).Int64()
	if err != nil {
		return 0, err
	}
	done, err := redisClient.SCard(ctx, FanoutDoneKey(knowledgeID, generation)).Result()
	if err != nil {
		return 0, err
	}
	remaining := total - done
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

func ClearFanIn(ctx context.Context, redisClient *redis.Client, knowledgeID, generation string) error {
	if redisClient == nil {
		return nil
	}
	return redisClient.Del(
		ctx,
		MultimodalPendingKey(knowledgeID, generation),
		FanoutDoneKey(knowledgeID, generation),
	).Err()
}

func EnqueuePostProcess(
	enqueuer interfaces.TaskEnqueuer,
	payload types.KnowledgePostProcessPayload,
) error {
	return EnqueuePostProcessContext(context.Background(), enqueuer, payload)
}

func EnqueuePostProcessContext(
	ctx context.Context,
	enqueuer interfaces.TaskEnqueuer,
	payload types.KnowledgePostProcessPayload,
) error {
	if enqueuer == nil {
		return errors.New("postprocess task enqueuer is unavailable")
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal post-process payload: %w", err)
	}
	task := asynq.NewTask(types.TypeKnowledgePostProcess, payloadBytes)
	if _, err := EnqueueStableTask(
		ctx,
		enqueuer,
		task,
		types.QueueDefault,
		PostProcessTaskID(payload.KnowledgeID, payload.ProcessingGeneration),
		asynq.MaxRetry(3),
		asynq.Timeout(GenerationTaskTimeout),
		asynq.Retention(GenerationTaskRetention),
	); err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
		return err
	}
	return nil
}

// DispatchFanout replays the whole durable plan. Task IDs make replay
// idempotent while a real enqueue failure is returned so the core task retries.
func DispatchFanout(
	ctx context.Context,
	enqueuer interfaces.TaskEnqueuer,
	redisClient *redis.Client,
	plan FanoutPlan,
	completionStores ...DurableFanoutCompletionStore,
) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if enqueuer == nil {
		return errors.New("document fanout task enqueuer is unavailable")
	}

	if plan.itemCount() == 0 {
		payload := types.KnowledgePostProcessPayload{
			TracingContext:       plan.Tracing,
			TenantID:             plan.TenantID,
			KnowledgeID:          plan.KnowledgeID,
			KnowledgeBaseID:      plan.KnowledgeBaseID,
			ProcessingGeneration: plan.ProcessingGeneration,
			Language:             plan.Language,
			Attempt:              plan.Attempt,
		}
		if err := EnqueuePostProcess(enqueuer, payload); err != nil {
			return fmt.Errorf("enqueue post-process fanout: %w", err)
		}
		return nil
	}

	postProcessPayload := types.KnowledgePostProcessPayload{
		TracingContext:       plan.Tracing,
		TenantID:             plan.TenantID,
		KnowledgeID:          plan.KnowledgeID,
		KnowledgeBaseID:      plan.KnowledgeBaseID,
		ProcessingGeneration: plan.ProcessingGeneration,
		Language:             plan.Language,
		Attempt:              plan.Attempt,
	}
	hasDurableStore := len(completionStores) > 0 && completionStores[0] != nil
	completedFanoutItems := make(map[string]struct{})
	if hasDurableStore {
		completed, err := completionStores[0].ListKnowledgeFanoutCompletions(
			ctx,
			plan.TenantID,
			plan.KnowledgeID,
			plan.KnowledgeBaseID,
			plan.ProcessingGeneration,
		)
		if err != nil {
			return fmt.Errorf("restore durable fan-in progress: %w", err)
		}
		validCompleted := validCompletedFanoutItems(plan, completed)
		for _, item := range validCompleted {
			completedFanoutItems[item] = struct{}{}
		}
		remaining := int64(plan.itemCount() - len(validCompleted))
		mirrorFanoutCache(ctx, redisClient, plan, validCompleted)
		if remaining <= 0 {
			if err := EnqueuePostProcess(enqueuer, postProcessPayload); err != nil {
				return fmt.Errorf("replay durable completed fan-in postprocess: %w", err)
			}
			return ClearFanIn(ctx, redisClient, plan.KnowledgeID, plan.ProcessingGeneration)
		}
	}
	fanoutCount := plan.itemCount()
	if redisClient != nil && !hasDurableStore {
		// A core-task retry replays this plan after some image tasks may have
		// already decremented the counter. Replacing the live value with the
		// original image count would wait forever: completed stable-ID image
		// tasks do not run again. SetNX preserves the in-progress fan-in state.
		if err := redisClient.SetNX(
			ctx,
			MultimodalPendingKey(plan.KnowledgeID, plan.ProcessingGeneration),
			fanoutCount,
			FanInTTL,
		).Err(); err != nil {
			return fmt.Errorf("initialize multimodal fan-in counter: %w", err)
		}
		if err := redisClient.Expire(
			ctx, MultimodalPendingKey(plan.KnowledgeID, plan.ProcessingGeneration), FanInTTL,
		).Err(); err != nil {
			return fmt.Errorf("extend multimodal fan-in counter: %w", err)
		}
		storedCount, err := redisClient.Get(
			ctx, MultimodalPendingKey(plan.KnowledgeID, plan.ProcessingGeneration),
		).Int()
		if err != nil {
			return fmt.Errorf("read multimodal fan-in counter: %w", err)
		}
		if storedCount != fanoutCount {
			return fmt.Errorf("multimodal fan-in plan count changed: stored=%d planned=%d", storedCount, fanoutCount)
		}
		remaining, err := FanoutRemaining(ctx, redisClient, plan.KnowledgeID, plan.ProcessingGeneration)
		if err != nil {
			return fmt.Errorf("read multimodal fan-in progress: %w", err)
		}
		if remaining <= 0 {
			if err := EnqueuePostProcess(enqueuer, postProcessPayload); err != nil {
				return fmt.Errorf("replay completed fan-in postprocess: %w", err)
			}
			return ClearFanIn(ctx, redisClient, plan.KnowledgeID, plan.ProcessingGeneration)
		}
	}

	var enqueueErr error
	if plan.DataTable != nil {
		if _, completed := completedFanoutItems[DataTableFanoutItem()]; !completed {
			payload := types.DataTableSummaryPayload{
				TracingContext:       plan.Tracing,
				TenantID:             plan.TenantID,
				KnowledgeID:          plan.KnowledgeID,
				KnowledgeBaseID:      plan.KnowledgeBaseID,
				ProcessingGeneration: plan.ProcessingGeneration,
				SummaryModel:         plan.DataTable.SummaryModel,
				EmbeddingModel:       plan.DataTable.EmbeddingModel,
				Language:             plan.Language,
				Attempt:              plan.Attempt,
			}
			payloadBytes, err := json.Marshal(payload)
			if err != nil {
				enqueueErr = errors.Join(enqueueErr, fmt.Errorf("marshal data-table fanout: %w", err))
			} else {
				task := asynq.NewTask(types.TypeDataTableSummary, payloadBytes)
				if _, err := EnqueueStableTask(
					ctx,
					enqueuer,
					task,
					types.QueueDefault,
					DataTableSummaryTaskID(plan.KnowledgeID, plan.ProcessingGeneration),
					asynq.MaxRetry(3),
					asynq.Timeout(GenerationTaskTimeout),
					asynq.Retention(GenerationTaskRetention),
				); err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
					enqueueErr = errors.Join(enqueueErr, fmt.Errorf("enqueue data-table fanout: %w", err))
				}
			}
		}
	}

	for _, image := range plan.Images {
		if _, completed := completedFanoutItems[ImageFanoutItem(image.Index)]; completed {
			continue
		}
		payload := types.ImageMultimodalPayload{
			TracingContext:       plan.Tracing,
			TenantID:             plan.TenantID,
			KnowledgeID:          plan.KnowledgeID,
			KnowledgeBaseID:      plan.KnowledgeBaseID,
			ProcessingGeneration: plan.ProcessingGeneration,
			ChunkID:              image.ChunkID,
			ImageURL:             image.ImageURL,
			EnableOCR:            true,
			EnableCaption:        true,
			Language:             plan.Language,
			ImageSourceType:      image.ImageSourceType,
			Attempt:              plan.Attempt,
			ImageIndex:           image.Index,
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			enqueueErr = errors.Join(enqueueErr, fmt.Errorf("marshal image fanout %d: %w", image.Index, err))
			continue
		}
		task := asynq.NewTask(
			types.TypeImageMultimodal,
			payloadBytes,
		)
		if _, err := EnqueueStableTask(
			ctx,
			enqueuer,
			task,
			types.QueueMultimodal,
			ImageTaskID(plan.KnowledgeID, plan.ProcessingGeneration, image.Index),
			asynq.MaxRetry(3),
			asynq.Timeout(GenerationTaskTimeout),
			asynq.Retention(GenerationTaskRetention),
		); err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
			enqueueErr = errors.Join(enqueueErr, fmt.Errorf("enqueue image fanout %d: %w", image.Index, err))
		}
	}
	return enqueueErr
}
