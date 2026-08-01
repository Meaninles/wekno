package modeladmission

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type streamTestChat struct {
	stream chan types.StreamResponse
}

func (*streamTestChat) Chat(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (*types.ChatResponse, error) {
	return &types.ChatResponse{}, nil
}

func (c *streamTestChat) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return c.stream, nil
}

func (*streamTestChat) GetModelName() string { return "stream-test" }
func (*streamTestChat) GetModelID() string   { return "stream-test-id" }

type failingTestChat struct {
	err error
}

func (c *failingTestChat) Chat(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (*types.ChatResponse, error) {
	return nil, c.err
}

func (c *failingTestChat) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, c.err
}

func (*failingTestChat) GetModelName() string { return "failing-test" }
func (*failingTestChat) GetModelID() string   { return "failing-test-id" }

func TestEffectiveChatParallelismUsesHotDocumentGrant(t *testing.T) {
	config := testConfig(Limit{
		Concurrency: 8, Background: 3, PerTenant: 4, PerDocument: 2,
	})
	manager := newManagerWithConfig(nil, config)
	wrapped := WrapChat(
		manager,
		Spec{Kind: KindChat, Domain: "fanout", TenantID: 1},
		&streamTestChat{},
	)
	require.Equal(t, 2, EffectiveChatParallelism(context.Background(), wrapped, 4))
	require.Equal(t, 1, EffectiveChatParallelism(context.Background(), wrapped, 1))
	require.Equal(t, 4, EffectiveChatParallelism(context.Background(), &streamTestChat{}, 4))
}

func TestProviderTransportFailureIsTypedWithoutLosingCause(t *testing.T) {
	config := testConfig(Limit{Concurrency: 1, PerTenant: 1})
	manager := newManagerWithConfig(nil, config)
	rawErr := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	wrapped := WrapChat(
		manager,
		Spec{Kind: KindChat, Domain: "outage", TenantID: 1},
		&failingTestChat{err: rawErr},
	)

	_, err := wrapped.Chat(context.Background(), nil, nil)
	require.ErrorIs(t, err, rawErr)
	require.ErrorIs(t, err, ErrProviderUnavailable)
	require.True(t, IsProviderUnavailable(err))
	retryAfter, ok := ProviderRetryAfter(err)
	require.True(t, ok)
	require.Equal(t, 15*time.Second, retryAfter)
	require.EqualValues(t, 0, manager.Snapshot().InFlight)
}

func TestSemanticModelFailureStillConsumesNormalRetryBudget(t *testing.T) {
	config := testConfig(Limit{Concurrency: 1, PerTenant: 1})
	manager := newManagerWithConfig(nil, config)
	rawErr := errors.New("decode structured response: invalid JSON")
	wrapped := WrapChat(
		manager,
		Spec{Kind: KindChat, Domain: "semantic", TenantID: 1},
		&failingTestChat{err: rawErr},
	)

	_, err := wrapped.Chat(context.Background(), nil, nil)
	require.ErrorIs(t, err, rawErr)
	require.False(t, IsProviderUnavailable(err))
	_, ok := ProviderRetryAfter(err)
	require.False(t, ok)
}

func TestAdmissionGrantedHookRunsOnlyAfterCapacityIsAcquired(t *testing.T) {
	manager := newManagerWithConfig(nil, testConfig(Limit{Concurrency: 1, PerTenant: 1}))
	spec := Spec{Kind: KindChat, Domain: "hook-boundary", TenantID: 1}
	firstLease, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)

	var calls atomic.Int32
	ctx := WithAdmissionGrantedHook(context.Background(), func(context.Context) error {
		calls.Add(1)
		return nil
	})
	wrapped := WrapChat(manager, spec, &streamTestChat{})
	_, err = wrapped.Chat(ctx, nil, nil)
	require.Error(t, err)
	require.Zero(t, calls.Load())

	firstLease.Release()
	_, err = wrapped.Chat(ctx, nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
}

func TestAdmissionGrantedHookFailureReleasesCapacityWithoutCallingProvider(t *testing.T) {
	manager := newManagerWithConfig(nil, testConfig(Limit{Concurrency: 1, PerTenant: 1}))
	spec := Spec{Kind: KindChat, Domain: "hook-failure", TenantID: 1}
	hookErr := errors.New("persist durable provider boundary")
	ctx := WithAdmissionGrantedHook(context.Background(), func(context.Context) error {
		return hookErr
	})
	wrapped := WrapChat(manager, spec, &streamTestChat{})

	_, err := wrapped.Chat(ctx, nil, nil)
	require.ErrorIs(t, err, hookErr)
	require.EqualValues(t, 0, manager.Snapshot().InFlight)
}

func TestChatStreamHoldsAdmissionUntilProducerCloses(t *testing.T) {
	manager := newManagerWithConfig(nil, testConfig(Limit{Concurrency: 1, PerTenant: 1}))
	inner := &streamTestChat{stream: make(chan types.StreamResponse, 1)}
	wrapped := WrapChat(
		manager,
		Spec{Kind: KindChat, Domain: "stream", TenantID: 1},
		inner,
	)

	output, err := wrapped.ChatStream(context.Background(), nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, manager.Snapshot().InFlight)
	inner.stream <- types.StreamResponse{}
	close(inner.stream)
	for range output {
	}
	require.Eventually(t, func() bool {
		return manager.Snapshot().InFlight == 0
	}, time.Second, 10*time.Millisecond)
}

func TestChatStreamEmitsTerminalErrorWhenDistributedLeaseIsLost(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not configured")
	}
	client := configuredRedisTestClient(address)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	config := testConfig(Limit{Concurrency: 1, PerTenant: 1})
	config.KeyPrefix = "test:model-admission:" + uuid.NewString() + ":"
	config.LeaseTTL = 600 * time.Millisecond
	config.HeartbeatInterval = 40 * time.Millisecond
	manager := newManagerWithConfig(client, config)
	spec := Spec{Kind: KindChat, Domain: "stream-fence", TenantID: 1}
	inner := &streamTestChat{stream: make(chan types.StreamResponse)}
	wrapped := WrapChat(manager, spec, inner)

	output, err := wrapped.ChatStream(context.Background(), nil, nil)
	require.NoError(t, err)
	keys := manager.keys(spec)
	t.Cleanup(func() {
		_ = client.Del(context.Background(), keys.total, keys.background, keys.tenant, keys.rate).Err()
	})
	require.NoError(t, client.Del(context.Background(), keys.total).Err())
	select {
	case response := <-output:
		require.Equal(t, types.ResponseTypeError, response.ResponseType)
		require.True(t, response.Done)
		require.Contains(t, response.Content, ErrAdmissionLeaseLost.Error())
	case <-time.After(time.Second):
		t.Fatal("stream did not surface distributed admission lease loss")
	}
	require.Eventually(t, func() bool {
		return manager.Snapshot().InFlight == 0
	}, time.Second, 10*time.Millisecond)
}

type pooledTestEmbedder struct {
	active    atomic.Int32
	maxActive atomic.Int32
}

func (e *pooledTestEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	result, err := e.BatchEmbed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return result[0], nil
}

func (e *pooledTestEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	active := e.active.Add(1)
	for {
		maximum := e.maxActive.Load()
		if active <= maximum || e.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer e.active.Add(-1)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = []float32{float32(index)}
	}
	return result, nil
}

func (e *pooledTestEmbedder) BatchEmbedWithPool(
	ctx context.Context,
	model embedding.Embedder,
	texts []string,
) ([][]float32, error) {
	var wg sync.WaitGroup
	results := make([][]float32, len(texts))
	errs := make(chan error, len(texts))
	for index, text := range texts {
		wg.Add(1)
		go func(index int, text string) {
			defer wg.Done()
			value, err := model.BatchEmbed(ctx, []string{text})
			if err != nil {
				errs <- err
				return
			}
			results[index] = value[0]
		}(index, text)
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}
	return results, nil
}

func (*pooledTestEmbedder) GetModelName() string { return "pooled-test" }
func (*pooledTestEmbedder) GetDimensions() int   { return 1 }
func (*pooledTestEmbedder) GetModelID() string   { return "pooled-test-id" }

func TestPooledEmbeddingCannotBypassAdmissionWrapper(t *testing.T) {
	config := testConfig(Limit{Concurrency: 1, PerTenant: 1})
	// The assertion is about wrapper re-entry, not the 100 ms timeout used by
	// the manager timeout tests. Under `go test ./...`, package-level CPU
	// contention can legitimately stretch three serialized 20 ms calls.
	config.InteractiveMaxWait = 2 * time.Second
	manager := newManagerWithConfig(nil, config)
	inner := &pooledTestEmbedder{}
	wrapped := WrapEmbedder(
		manager,
		Spec{Kind: KindEmbedding, Domain: "embedding", TenantID: 1},
		inner,
	)

	result, err := wrapped.BatchEmbedWithPool(
		context.Background(),
		wrapped,
		[]string{"one", "two", "three"},
	)
	require.NoError(t, err)
	require.Len(t, result, 3)
	require.EqualValues(t, 1, inner.maxActive.Load())
	require.EqualValues(t, 0, manager.Snapshot().InFlight)
}
