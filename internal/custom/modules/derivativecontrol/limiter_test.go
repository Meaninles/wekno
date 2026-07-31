package derivativecontrol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

type blockingChat struct {
	id      string
	started chan struct{}
	release chan struct{}
	once    sync.Once
	active  atomic.Int32
	max     atomic.Int32
	err     error
	usage   int
}

func (c *blockingChat) Chat(
	ctx context.Context, _ []chat.Message, _ *chat.ChatOptions,
) (*types.ChatResponse, error) {
	active := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		previous := c.max.Load()
		if active <= previous || c.max.CompareAndSwap(previous, active) {
			break
		}
	}
	if c.started != nil {
		c.once.Do(func() { close(c.started) })
	}
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	return &types.ChatResponse{
		Content: "ok",
		Usage:   types.TokenUsage{TotalTokens: c.usage},
	}, nil
}

func (c *blockingChat) ChatStream(
	context.Context, []chat.Message, *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	out := make(chan types.StreamResponse)
	close(out)
	return out, nil
}

func (c *blockingChat) GetModelName() string { return c.id }
func (c *blockingChat) GetModelID() string   { return c.id }

func TestLimiterDoesNotCreateGlobalLocalLease(t *testing.T) {
	limiter := NewLimiter(nil, &derivativeSettingsStub{tpm: 60_000})

	first, err := limiter.acquire(context.Background(), "actual-model-a")
	require.NoError(t, err)
	second, err := limiter.acquire(context.Background(), "actual-model-a")
	require.NoError(t, err)
	third, err := limiter.acquire(context.Background(), "actual-model-b")
	require.NoError(t, err)

	first.release()
	second.release()
	third.release()
	require.EqualValues(t, 3, limiter.Snapshot(context.Background()).Acquired)
	require.EqualValues(t, 0, limiter.Snapshot(context.Background()).Deferred)
}

func redisForDerivativeTest(t *testing.T) *redis.Client {
	t.Helper()
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not set")
	}
	password := os.Getenv("WEKNORA_TEST_REDIS_PASSWORD")
	if password == "" {
		password = os.Getenv("REDIS_PASSWORD")
	}
	db, _ := strconv.Atoi(os.Getenv("REDIS_DB"))
	client := redis.NewClient(&redis.Options{
		Addr: address, Password: password, DB: db,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err())
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func cleanLimiterKeys(t *testing.T, client *redis.Client, limiter *Limiter, poolKeys ...string) {
	t.Helper()
	keys := make([]string, 0, len(poolKeys))
	for _, poolKey := range poolKeys {
		keys = append(keys, limiter.key("pool:"+poolKey+":pace"))
	}
	require.NoError(t, client.Del(context.Background(), keys...).Err())
	t.Cleanup(func() {
		_ = client.Del(context.Background(), keys...).Err()
	})
}

func TestLimiterRedisTPMIsSharedPerActualModelAndIndependentAcrossModels(t *testing.T) {
	client := redisForDerivativeTest(t)
	t.Setenv(
		"WEKNORA_REDIS_NAMESPACE",
		fmt.Sprintf("derivative-test-%d", time.Now().UnixNano()),
	)
	settings := &derivativeSettingsStub{tpm: 60_000}
	first := NewLimiter(client, settings)
	second := NewLimiter(client, settings)
	cleanLimiterKeys(t, client, first, "actual-model-a", "actual-model-b")

	firstLease, err := first.acquire(context.Background(), "actual-model-a")
	require.NoError(t, err)
	require.NoError(t, firstLease.pace(context.Background(), 120))
	firstLease.release()

	secondLease, err := second.acquire(context.Background(), "actual-model-a")
	require.NoError(t, err)
	err = secondLease.pace(context.Background(), 120)
	var deferred *DeferredError
	require.ErrorAs(t, err, &deferred)
	require.Greater(t, deferred.RetryAfter, 50*time.Millisecond)
	require.LessOrEqual(t, deferred.RetryAfter, 150*time.Millisecond)
	secondLease.release()

	independentLease, err := second.acquire(context.Background(), "actual-model-b")
	require.NoError(t, err)
	require.NoError(t, independentLease.pace(context.Background(), 120))
	independentLease.release()

	time.Sleep(140 * time.Millisecond)
	secondLease, err = second.acquire(context.Background(), "actual-model-a")
	require.NoError(t, err)
	require.NoError(t, secondLease.pace(context.Background(), 120))
	secondLease.release()
}

func TestLimiterActualUsageAndTimeoutExtendGlobalPacing(t *testing.T) {
	client := redisForDerivativeTest(t)
	t.Setenv(
		"WEKNORA_REDIS_NAMESPACE",
		fmt.Sprintf("derivative-usage-test-%d", time.Now().UnixNano()),
	)
	settings := &derivativeSettingsStub{tpm: 60_000}
	limiter := NewLimiter(client, settings)
	quickPool := poolKeyForName("quick")
	timeoutPool := poolKeyForName("timeout")
	cleanLimiterKeys(t, client, limiter, quickPool, timeoutPool)

	quick := limiter.Wrap(&blockingChat{id: "quick", usage: 120})
	_, err := quick.Chat(
		context.Background(),
		[]chat.Message{{Role: "user", Content: "x"}},
		&chat.ChatOptions{MaxTokens: 1},
	)
	require.NoError(t, err)
	nextRaw, err := client.Get(
		context.Background(), limiter.key("pool:"+quickPool+":pace"),
	).Result()
	require.NoError(t, err)
	nextAt, err := strconv.ParseInt(nextRaw, 10, 64)
	require.NoError(t, err)
	require.Greater(t, nextAt-time.Now().UnixMilli(), int64(50))
	require.Less(t, nextAt-time.Now().UnixMilli(), int64(250))

	timeoutModel := limiter.Wrap(&blockingChat{
		id: "timeout", err: context.DeadlineExceeded,
	})
	_, err = timeoutModel.Chat(
		context.Background(),
		[]chat.Message{{Role: "user", Content: "x"}},
		&chat.ChatOptions{MaxTokens: 1},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	nextRaw, err = client.Get(
		context.Background(), limiter.key("pool:"+timeoutPool+":pace"),
	).Result()
	require.NoError(t, err)
	nextAt, err = strconv.ParseInt(nextRaw, 10, 64)
	require.NoError(t, err)
	require.Greater(t, nextAt-time.Now().UnixMilli(), int64((9 * time.Minute).Milliseconds()))
	require.LessOrEqual(t, nextAt-time.Now().UnixMilli(), int64((11 * time.Minute).Milliseconds()))
}

func TestLimiterRedisFailureFailsClosed(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  30 * time.Millisecond,
		ReadTimeout:  30 * time.Millisecond,
		WriteTimeout: 30 * time.Millisecond,
		MaxRetries:   0,
	})
	t.Cleanup(func() { _ = client.Close() })
	limiter := NewLimiter(client, &derivativeSettingsStub{tpm: 20_000})

	lease, err := limiter.acquire(context.Background(), "actual-model")
	require.NoError(t, err)
	err = lease.pace(context.Background(), 1)
	var deferred *DeferredError
	require.ErrorAs(t, err, &deferred)
	require.Contains(t, deferred.Reason, "TPM pacing failed closed")
	require.NotNil(t, deferred.Cause)
	require.False(t, errors.Is(err, context.Canceled))
	lease.release()
}
