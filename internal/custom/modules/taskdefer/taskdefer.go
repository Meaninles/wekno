// Package taskdefer bridges an Asynq retry-limit edge case for durable,
// generation-scoped model work.
//
// Asynq checks Retried >= MaxRetry before consulting Config.IsFailure. A
// provider outage on a task that had already spent its business retry budget
// would therefore be archived even though the outage is explicitly classified
// as budget-free. At that exact boundary this middleware ACKs the old delivery
// only after a delayed resume wake-up has been accepted by Redis. The wake-up
// republishes the original stable task ID; PostgreSQL completion proofs and
// generation fences keep the operation exactly-once at the business boundary.
package taskdefer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hibiken/asynq"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	// TypeResume is consumed only by the isolated control queue. It performs no
	// model call and never executes document business logic itself.
	TypeResume = "custom:model_task_resume"

	resumePayloadVersion = 1
	resumeMaxRetry       = 10_000
	resumeTaskTimeout    = time.Minute
	resumeConflictDelay  = 15 * time.Second
	cancelResumeDelay    = 2 * time.Second
	resumeJitterWindow   = 5 * time.Second
)

// ErrOriginalTaskLive is a serialization outcome, not a failed resume. The
// control task waits until the old active delivery has either ACKed or lost
// its lease before replacing the same stable task ID.
var ErrOriginalTaskLive = errors.New("durable model task original delivery is still live")

type resumePayload struct {
	Version     int               `json:"version"`
	TaskType    string            `json:"task_type"`
	TaskPayload []byte            `json:"task_payload"`
	Headers     map[string]string `json:"headers,omitempty"`
	Queue       string            `json:"queue"`
	TaskID      string            `json:"task_id"`
	MaxRetry    int               `json:"max_retry"`
}

func (p resumePayload) validate() error {
	if p.Version != resumePayloadVersion ||
		!replayableModelLeaf(p.TaskType) ||
		len(p.TaskPayload) == 0 ||
		strings.TrimSpace(p.Queue) == "" ||
		strings.TrimSpace(p.TaskID) == "" ||
		p.MaxRetry < 0 {
		return errors.New("durable model task resume payload is invalid")
	}
	return nil
}

// Middleware returns a handler wrapper that closes the retry=max gap. It is a
// no-op for normal business failures, model deferrals below the retry limit,
// and tasks whose durable replay is owned by another PostgreSQL state machine
// (document roots, physical split parts and Wiki operations).
func Middleware(enqueuer interfaces.TaskEnqueuer) asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
			err := next.ProcessTask(ctx, task)
			if err == nil || task == nil ||
				(!modeladmission.IsModelWorkDeferred(err) && !errors.Is(err, context.Canceled)) {
				return err
			}
			retried, retryOK := asynq.GetRetryCount(ctx)
			maxRetry, maxOK := asynq.GetMaxRetry(ctx)
			taskID, taskIDOK := asynq.GetTaskID(ctx)
			queue, queueOK := asynq.GetQueueName(ctx)
			if !retryOK || !maxOK || retried < maxRetry ||
				!taskIDOK || !queueOK || !replayableModelLeaf(task.Type()) {
				return err
			}

			delay := cancelResumeDelay
			if providerDelay, ok := modeladmission.ModelRetryAfter(err); ok {
				delay = providerDelay
				if delay < time.Second {
					delay = time.Second
				}
			}
			delay += stableJitter(task)
			if scheduleErr := scheduleResume(
				enqueuer, task, queue, taskID, maxRetry, delay,
			); scheduleErr != nil {
				return errors.Join(err, fmt.Errorf(
					"persist durable model task resume queue=%s id=%s: %w",
					queue, taskID, scheduleErr,
				))
			}
			logger.Warnf(ctx,
				"[task defer] persisted model-infrastructure resume type=%s queue=%s id=%s retry=%d/%d delay=%s",
				task.Type(), queue, taskID, retried, maxRetry, delay,
			)
			// The replacement wake-up is durable before the old delivery is
			// ACKed. A crash before Asynq removes the old active record causes a
			// redelivery; Unique coalesces its identical wake-up.
			return nil
		})
	}
}

func scheduleResume(
	enqueuer interfaces.TaskEnqueuer,
	original *asynq.Task,
	queue string,
	taskID string,
	maxRetry int,
	delay time.Duration,
) error {
	if enqueuer == nil {
		return errors.New("durable model task resume enqueuer is unavailable")
	}
	if original == nil {
		return errors.New("durable model task resume original task is nil")
	}
	payload := resumePayload{
		Version:     resumePayloadVersion,
		TaskType:    original.Type(),
		TaskPayload: append([]byte(nil), original.Payload()...),
		Headers:     original.Headers(),
		Queue:       strings.TrimSpace(queue),
		TaskID:      strings.TrimSpace(taskID),
		MaxRetry:    maxRetry,
	}
	if err := payload.validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode durable model task resume: %w", err)
	}
	if delay < time.Second {
		delay = time.Second
	}
	_, err = enqueuer.Enqueue(
		asynq.NewTask(TypeResume, encoded),
		asynq.Queue(types.QueueCritical),
		asynq.ProcessIn(delay),
		asynq.MaxRetry(resumeMaxRetry),
		asynq.Timeout(resumeTaskTimeout),
		// The uniqueness lease includes the old task's maximum active lease.
		// Asynq releases it immediately when the resume succeeds.
		asynq.Unique(delay+processownership.GenerationTaskTimeout+time.Minute),
	)
	if errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return err
}

// Handler republishes a deferred generation leaf under its original stable ID.
type Handler struct {
	enqueuer interfaces.TaskEnqueuer
}

func NewHandler(enqueuer interfaces.TaskEnqueuer) *Handler {
	return &Handler{enqueuer: enqueuer}
}

func (h *Handler) Handle(ctx context.Context, task *asynq.Task) error {
	if h == nil || h.enqueuer == nil {
		return errors.New("durable model task resume handler is unavailable")
	}
	if task == nil {
		return errors.New("durable model task resume task is nil")
	}
	var payload resumePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("decode durable model task resume: %w", err)
	}
	if err := payload.validate(); err != nil {
		return err
	}
	original := asynq.NewTaskWithHeaders(
		payload.TaskType,
		payload.TaskPayload,
		payload.Headers,
	)
	_, err := processownership.EnqueueStableTask(
		ctx,
		h.enqueuer,
		original,
		payload.Queue,
		payload.TaskID,
		asynq.MaxRetry(payload.MaxRetry),
		asynq.Timeout(processownership.GenerationTaskTimeout),
		asynq.Retention(processownership.GenerationTaskRetention),
	)
	if err == nil {
		logger.Infof(ctx,
			"[task defer] resumed model task type=%s queue=%s id=%s",
			payload.TaskType, payload.Queue, payload.TaskID,
		)
		return nil
	}
	if !errors.Is(err, asynq.ErrTaskIDConflict) {
		return fmt.Errorf("republish durable model task: %w", err)
	}

	required, complete, checkErr := processownership.StableTaskCompletionState(
		ctx, h.enqueuer, original,
	)
	if checkErr != nil {
		return fmt.Errorf("check durable model task completion: %w", checkErr)
	}
	if required && complete {
		// The old delivery committed between the republish conflict and this
		// proof read. No replacement is needed.
		return nil
	}
	return ErrOriginalTaskLive
}

func replayableModelLeaf(taskType string) bool {
	switch taskType {
	case types.TypeChunkExtract,
		types.TypeQuestionGeneration,
		types.TypeSummaryGeneration,
		types.TypeImageMultimodal,
		types.TypeDataTableSummary:
		return true
	default:
		return false
	}
}

func stableJitter(task *asynq.Task) time.Duration {
	if task == nil || resumeJitterWindow <= 0 {
		return 0
	}
	digest := sha256.Sum256(append(
		append([]byte(task.Type()), 0),
		task.Payload()...,
	))
	windowMillis := uint64(resumeJitterWindow / time.Millisecond)
	if windowMillis == 0 {
		return 0
	}
	return time.Duration(binary.BigEndian.Uint64(digest[:8])%windowMillis) * time.Millisecond
}

func RetryDelay(err error) (time.Duration, bool) {
	if errors.Is(err, ErrOriginalTaskLive) {
		return resumeConflictDelay, true
	}
	return 0, false
}
