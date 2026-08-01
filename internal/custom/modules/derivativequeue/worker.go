package derivativequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/dig"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const workLeaseTTL = 45 * time.Second

type generationOutcomeFinalizer interface {
	FinalizeSubtaskGenerationItemOutcome(
		context.Context, uint64, string, string, string, string, string, string,
	) (int, bool, error)
}

type generationZeroFinalizer interface {
	FinalizeSubtaskGenerationItem(
		context.Context, uint64, string, string, string, string,
	) (int, bool, error)
}

type finalizerWaitError struct{}

func (finalizerWaitError) Error() string                  { return "derivative siblings are not terminal" }
func (finalizerWaitError) ModelWorkDeferred() bool        { return true }
func (finalizerWaitError) ModelRetryAfter() time.Duration { return 5 * time.Second }

type Worker struct {
	repository       *Repository
	knowledgeService interfaces.KnowledgeService
	chunkExtractor   interfaces.TaskHandler
	dataTableSummary interfaces.TaskHandler
	knowledgeRepo    interfaces.KnowledgeRepository
	owner            string
}

type WorkerParams struct {
	dig.In

	Repository       *Repository
	KnowledgeService interfaces.KnowledgeService
	ChunkExtractor   interfaces.TaskHandler `name:"chunkExtractor"`
	DataTableSummary interfaces.TaskHandler `name:"dataTableSummary"`
}

func NewWorker(params WorkerParams) *Worker {
	owner, _ := os.Hostname()
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = "derivative-worker"
	}
	return &Worker{
		repository: params.Repository, knowledgeService: params.KnowledgeService,
		chunkExtractor:   params.ChunkExtractor,
		dataTableSummary: params.DataTableSummary,
		knowledgeRepo:    params.KnowledgeService.GetRepository(),
		owner:            owner + ":" + uuid.NewString(),
	}
}

func (w *Worker) Handle(ctx context.Context, task *asynq.Task) error {
	if w == nil || w.repository == nil {
		return errors.New("durable derivative worker is unavailable")
	}
	var wake WakePayload
	if err := json.Unmarshal(task.Payload(), &wake); err != nil ||
		strings.TrimSpace(wake.WorkItemID) == "" || wake.DispatchEpoch == 0 {
		logger.Warnf(ctx, "[derivative queue] discard malformed wake payload: %v", err)
		return nil
	}
	item, err := w.repository.Claim(ctx, wake, w.owner, workLeaseTTL)
	if errors.Is(err, ErrStaleDispatch) || errors.Is(err, ErrInvalidState) ||
		errors.Is(err, ErrGenerationFence) || errors.Is(err, gorm.ErrRecordNotFound) {
		pipelineobs.DerivativeDuplicateWake()
		return nil
	}
	if err != nil {
		// PostgreSQL remains authoritative; ACK this wake and let the
		// maintenance dispatcher publish a fresh epoch.
		logger.Warnf(ctx, "[derivative queue] claim deferred work_item=%s: %v", wake.WorkItemID, err)
		return nil
	}

	execCtx := WithExecution(ctx, w.repository, item)
	execCtx = modeladmission.WithWorkLane(execCtx, modeladmission.WorkLaneDerivative)
	execCtx = modeladmission.WithAdmissionGrantedHook(
		execCtx,
		func(hookCtx context.Context) error {
			return BeginProviderForContext(hookCtx)
		},
	)
	if item.WorkKind != WorkFinalizer && w.repository.admission != nil {
		var releaseTaskWork func()
		execCtx, releaseTaskWork, err = w.repository.admission.AcquireTaskWork(
			execCtx,
			modeladmission.Spec{
				Kind:           modeladmission.KindDerivative,
				Domain:         item.ResourcePoolID,
				TenantID:       item.TenantID,
				ModelID:        item.ModelID,
				ModelTenantID:  item.ModelTenantID,
				DerivativeOnly: true,
				KnowledgeID:    item.KnowledgeID,
			},
			modeladmission.WorkLaneDerivative,
		)
		if err != nil {
			return w.deferFailure(execCtx, item, err)
		}
		defer releaseTaskWork()
	}
	execCtx, cancel := context.WithCancel(execCtx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go w.heartbeat(execCtx, item, heartbeatDone)

	businessErr := w.execute(execCtx, item)
	cancel()
	<-heartbeatDone

	if businessErr == nil {
		if item.WorkKind == WorkFinalizer {
			if err := w.repository.CompleteFinalizer(
				context.WithoutCancel(ctx), item.ID, item.LeaseToken,
			); err != nil {
				return w.deferFailure(execCtx, item, err)
			}
			return nil
		}
		if _, err := w.repository.SealProviderExecution(
			context.WithoutCancel(ctx), item.ID, item.LeaseToken,
		); err != nil {
			return w.deferFailure(execCtx, item, err)
		}
		outcomeStatus, outcomeDetail := outcomeFromContext(execCtx)
		if err := w.repository.CompleteMaterializationOutcome(
			context.WithoutCancel(ctx), item.ID, item.LeaseToken,
			outcomeStatus, outcomeDetail,
		); err != nil {
			return w.deferFailure(execCtx, item, err)
		}
		return nil
	}
	return w.deferFailure(execCtx, item, businessErr)
}

func (w *Worker) heartbeat(ctx context.Context, item *WorkItem, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.repository.Heartbeat(
				context.WithoutCancel(ctx), item.ID, item.LeaseToken, workLeaseTTL,
			); err != nil {
				logger.Warnf(ctx, "[derivative queue] heartbeat lost work_item=%s: %v", item.ID, err)
				return
			}
		}
	}
}

func (w *Worker) execute(ctx context.Context, item *WorkItem) error {
	taskType := ""
	var handler func(context.Context, *asynq.Task) error
	switch item.WorkKind {
	case WorkSummary:
		taskType = types.TypeSummaryGeneration
		handler = w.knowledgeService.ProcessSummaryGeneration
	case WorkQuestion:
		taskType = types.TypeQuestionGeneration
		handler = w.knowledgeService.ProcessQuestionGeneration
	case WorkGraph:
		taskType = types.TypeChunkExtract
		if w.chunkExtractor != nil {
			handler = w.chunkExtractor.Handle
		}
	case WorkDataTable:
		taskType = types.TypeDataTableSummary
		if w.dataTableSummary != nil {
			handler = w.dataTableSummary.Handle
		}
	case WorkFinalizer:
		return w.reconcileFinalizer(ctx, item)
	default:
		return fmt.Errorf("unsupported derivative work kind %q", item.WorkKind)
	}
	if handler == nil {
		return fmt.Errorf("derivative handler for %s is unavailable", item.WorkKind)
	}
	return handler(ctx, asynq.NewTask(taskType, append([]byte(nil), item.Payload...)))
}

func (w *Worker) reconcileFinalizer(ctx context.Context, item *WorkItem) error {
	siblings, ready, err := w.repository.FinalizerSiblings(ctx, *item)
	if err != nil {
		return err
	}
	if !ready {
		return finalizerWaitError{}
	}
	for _, sibling := range siblings {
		status, detail := "completed", ""
		switch sibling.State {
		case StateFailed, StateProviderUnknown:
			status, detail = "failed", sibling.LastErrorMessage
		case StateCancelled:
			// A generation change cancels the finalizer itself at Claim. A
			// same-generation cancellation is an explicit degraded outcome.
			status, detail = "failed", "derivative work item was cancelled"
		case StateCompleted:
			if sibling.OutcomeStatus == "degraded" {
				status, detail = "degraded", sibling.OutcomeDetail
			}
		}
		if err := w.finalize(ctx, &sibling, status, detail); err != nil {
			return err
		}
	}
	zero, ok := w.knowledgeRepo.(generationZeroFinalizer)
	if !ok || zero == nil {
		return errors.New("durable derivative zero-count finalizer is unavailable")
	}
	_, _, err = zero.FinalizeSubtaskGenerationItem(
		ctx, item.TenantID, item.KnowledgeID, item.KnowledgeBaseID,
		item.ProcessingGeneration, "postprocess_zero",
	)
	return err
}

type modelDeferred interface {
	ModelWorkDeferred() bool
	ModelRetryAfter() time.Duration
}

type providerRetryRequired interface {
	ProviderRetryRequired() bool
}

func (w *Worker) deferFailure(ctx context.Context, item *WorkItem, cause error) error {
	if errors.Is(cause, ErrGenerationFence) || errors.Is(cause, ErrLeaseLost) {
		return nil
	}
	persistCtx := context.WithoutCancel(ctx)
	delay := 5 * time.Second
	errorClass := "infrastructure"
	errorCode := "derivative_execution_failed"
	var deferred modelDeferred
	if errors.As(cause, &deferred) && deferred.ModelWorkDeferred() {
		delay = deferred.ModelRetryAfter()
		errorClass = "admission"
		errorCode = "model_deferred"
		if err := w.repository.DeferForAdmission(
			persistCtx, item.ID, item.LeaseToken,
			errorCode, cause.Error(), delay,
		); err == nil || errors.Is(err, ErrLeaseLost) {
			return nil
		} else if !errors.Is(err, ErrInvalidState) {
			logger.Errorf(ctx, "[derivative queue] persist budget-free admission defer failed work_item=%s: %v",
				item.ID, err)
			return nil
		}
	}
	if errors.Is(cause, context.Canceled) {
		delay = 10 * time.Second
		errorCode = "worker_interrupted"
	}
	forceProviderRetry := false
	var providerRetry providerRetryRequired
	if errors.As(cause, &providerRetry) && providerRetry.ProviderRetryRequired() {
		forceProviderRetry = true
		errorClass = "contract"
		errorCode = "provider_response_rejected"
	}
	hasCalls, countErr := w.repository.hasProviderCalls(persistCtx, item.ID)
	if countErr != nil {
		logger.Warnf(ctx, "[derivative queue] inspect provider checkpoints failed work_item=%s: %v",
			item.ID, countErr)
		hasCalls = providerStarted(ctx)
	}
	if !providerStarted(ctx) && !hasCalls {
		err := w.repository.DeferWithoutProviderAttempt(
			persistCtx, item.ID, item.LeaseToken,
			errorClass, errorCode, cause.Error(), delay,
		)
		if err != nil && !errors.Is(err, ErrLeaseLost) {
			logger.Errorf(ctx, "[derivative queue] persist pre-provider defer failed work_item=%s: %v",
				item.ID, err)
		}
		return nil
	}
	_, err := w.repository.RetryAfterFailure(
		persistCtx, item.ID, item.LeaseToken,
		errorClass, errorCode, cause.Error(), delay,
		forceProviderRetry,
	)
	if hasCalls {
		pipelineobs.DerivativeMaterializeRetry(item.WorkKind)
	}
	if err != nil {
		if !errors.Is(err, ErrLeaseLost) {
			logger.Errorf(ctx, "[derivative queue] persist retry failed work_item=%s: %v", item.ID, err)
		}
		return nil
	}
	// The leaf only persists its terminal work-item state. The plan finalizer
	// is the sole owner of generation outcome reconciliation; core parse
	// completion is already independent and processing_generation remains the
	// fence for every late artifact write.
	return nil
}

func (w *Worker) finalize(
	ctx context.Context,
	item *WorkItem,
	status, detail string,
) error {
	finalizer, ok := w.knowledgeRepo.(generationOutcomeFinalizer)
	if !ok || finalizer == nil {
		return errors.New("durable derivative generation finalizer is unavailable")
	}
	_, _, err := finalizer.FinalizeSubtaskGenerationItemOutcome(
		ctx,
		item.TenantID,
		item.KnowledgeID,
		item.KnowledgeBaseID,
		item.ProcessingGeneration,
		item.ItemID,
		status,
		detail,
	)
	return err
}
