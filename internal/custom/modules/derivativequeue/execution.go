package derivativequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

type executionContextKey struct{}

type providerContractRejectedError struct{ cause error }

func (e *providerContractRejectedError) Error() string {
	return fmt.Sprintf("provider response contract rejected: %v", e.cause)
}

func (e *providerContractRejectedError) Unwrap() error { return e.cause }
func (e *providerContractRejectedError) ProviderRetryRequired() bool {
	return true
}

// ProviderContractRejected marks a deterministic response-validation error as
// requiring a fresh provider attempt. This remains true even when another
// request in the same batch has a valid replayable checkpoint.
func ProviderContractRejected(cause error) error {
	if cause == nil {
		cause = errors.New("provider response violated the materialization contract")
	}
	return &providerContractRejectedError{cause: cause}
}

type executionContext struct {
	repository *Repository
	workItemID string
	leaseToken string

	startMu sync.Mutex
	started bool
}

// WithExecution attaches the durable provider checkpoint boundary to the
// business handler context. Only derivative workers create this context.
func WithExecution(ctx context.Context, repository *Repository, item *WorkItem) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if repository == nil || item == nil || item.ID == "" || item.LeaseToken == "" {
		return ctx
	}
	return context.WithValue(ctx, executionContextKey{}, &executionContext{
		repository: repository,
		workItemID: item.ID,
		leaseToken: item.LeaseToken,
	})
}

func executionFromContext(ctx context.Context) (*executionContext, bool) {
	if ctx == nil {
		return nil, false
	}
	execution, ok := ctx.Value(executionContextKey{}).(*executionContext)
	return execution, ok && execution != nil && execution.repository != nil
}

// IsDurableExecution reports whether finalization/retry ownership belongs to
// the PostgreSQL work-item handler instead of native Asynq retries.
func IsDurableExecution(ctx context.Context) bool {
	_, ok := executionFromContext(ctx)
	return ok
}

func chatRequestHash(modelID string, messages []chat.Message, options *chat.ChatOptions) (string, error) {
	material, err := json.Marshal(struct {
		ModelID  string            `json:"model_id"`
		Messages []chat.Message    `json:"messages"`
		Options  *chat.ChatOptions `json:"options,omitempty"`
	}{
		ModelID: strings.TrimSpace(modelID), Messages: messages, Options: options,
	})
	if err != nil {
		return "", fmt.Errorf("encode derivative provider request identity: %w", err)
	}
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:]), nil
}

// LookupChatCheckpoint returns a previously persisted provider response.
// Replayed calls bypass admission and token pacing because they consume no
// provider capacity.
func LookupChatCheckpoint(
	ctx context.Context,
	modelID string,
	messages []chat.Message,
	options *chat.ChatOptions,
) (*types.ChatResponse, bool, error) {
	execution, ok := executionFromContext(ctx)
	if !ok {
		return nil, false, nil
	}
	requestHash, err := chatRequestHash(modelID, messages, options)
	if err != nil {
		return nil, false, err
	}
	call, err := execution.repository.GetProviderCall(ctx, execution.workItemID, requestHash)
	if err != nil {
		return nil, false, err
	}
	if call == nil {
		return nil, false, nil
	}
	var response types.ChatResponse
	if err := json.Unmarshal(call.Response, &response); err != nil {
		return nil, false, fmt.Errorf("decode durable provider response checkpoint: %w", err)
	}
	return &response, true, nil
}

// BeginProviderForContext increments the provider-attempt budget exactly once
// for one durable delivery and only after admission succeeded.
func BeginProviderForContext(ctx context.Context) error {
	execution, ok := executionFromContext(ctx)
	if !ok {
		return nil
	}
	execution.startMu.Lock()
	defer execution.startMu.Unlock()
	if execution.started {
		return nil
	}
	if _, err := execution.repository.BeginProvider(
		ctx, execution.workItemID, execution.leaseToken,
	); err != nil {
		return err
	}
	execution.started = true
	return nil
}

// CheckpointChatResponse persists an immutable copy of the actual response
// before the caller can materialize it into chunks, graph data, or indexes.
func CheckpointChatResponse(
	ctx context.Context,
	modelID string,
	messages []chat.Message,
	options *chat.ChatOptions,
	response *types.ChatResponse,
) error {
	execution, ok := executionFromContext(ctx)
	if !ok || response == nil {
		return nil
	}
	requestHash, err := chatRequestHash(modelID, messages, options)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode derivative provider response checkpoint: %w", err)
	}
	if len(raw) > MaxPayloadSize {
		return errors.New("provider response checkpoint exceeds 256 KiB object-storage threshold")
	}
	_, err = execution.repository.SaveProviderCall(
		ctx,
		execution.workItemID,
		execution.leaseToken,
		requestHash,
		strings.TrimSpace(modelID),
		strings.TrimSpace(response.ProviderRequestID),
		raw,
	)
	return err
}

// RejectChatCheckpoint preserves an invalid provider response for audit while
// removing it from the replay set. This is only for deterministic response
// contract failures discovered after the limiter checkpointed the successful
// HTTP response; transport and downstream materialization errors must leave
// the checkpoint replayable.
func RejectChatCheckpoint(
	ctx context.Context,
	modelID string,
	messages []chat.Message,
	options *chat.ChatOptions,
	cause error,
) error {
	execution, ok := executionFromContext(ctx)
	if !ok {
		return nil
	}
	requestHash, err := chatRequestHash(modelID, messages, options)
	if err != nil {
		return err
	}
	detail := "provider response violated the materialization contract"
	if cause != nil {
		detail = cause.Error()
	}
	if err := execution.repository.RejectProviderCall(
		ctx,
		execution.workItemID,
		execution.leaseToken,
		requestHash,
		detail,
	); err != nil {
		return fmt.Errorf("reject durable provider response checkpoint: %w", err)
	}
	return nil
}

func providerStarted(ctx context.Context) bool {
	execution, ok := executionFromContext(ctx)
	if !ok {
		return false
	}
	execution.startMu.Lock()
	defer execution.startMu.Unlock()
	return execution.started
}
