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

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func unreachableRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  20 * time.Millisecond,
		ReadTimeout:  20 * time.Millisecond,
		WriteTimeout: 20 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func configuredRedisTestClient(address string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:       address,
		Password:   os.Getenv("WEKNORA_TEST_REDIS_PASSWORD"),
		MaxRetries: -1,
	})
}

func testConfig(limit Limit) Config {
	return Config{
		Enabled:            true,
		FailClosed:         true,
		KeyPrefix:          "test:model-admission:",
		LeaseTTL:           time.Second,
		HeartbeatInterval:  200 * time.Millisecond,
		InteractiveReserve: 0,
		InteractiveMaxWait: 100 * time.Millisecond,
		BackgroundMaxWait:  100 * time.Millisecond,
		Limits: map[Kind]Limit{
			KindChat:      limit,
			KindEmbedding: limit,
		},
	}
}

func TestLocalAdmissionEnforcesConcurrencyAndReleases(t *testing.T) {
	manager := newManagerWithConfig(nil, testConfig(Limit{Concurrency: 1, PerTenant: 1}))
	spec := Spec{Kind: KindChat, Domain: "shared", TenantID: 100}
	first, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err = manager.Acquire(waitCtx, spec)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	first.Release()
	next, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)
	next.Release()
	require.EqualValues(t, 0, manager.Snapshot().InFlight)
	require.EqualValues(t, 2, manager.Snapshot().Acquired)
}

func TestAdmissionMaxWaitCapsLongerParentDeadline(t *testing.T) {
	config := testConfig(Limit{Concurrency: 1, PerTenant: 1})
	config.InteractiveMaxWait = 40 * time.Millisecond
	manager := newManagerWithConfig(nil, config)
	spec := Spec{Kind: KindChat, Domain: "bounded-wait", TenantID: 100}
	first, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)
	defer first.Release()

	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err = manager.Acquire(parent, spec)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 300*time.Millisecond)
}

func TestBackgroundAdmissionIsNonBlockingAndReturnsRetryAfter(t *testing.T) {
	config := testConfig(Limit{Concurrency: 1, PerTenant: 1})
	config.BackgroundMaxWait = 0
	manager := newManagerWithConfig(nil, config)
	spec := Spec{Kind: KindChat, Domain: "durable-background", TenantID: 100}
	first, err := manager.Acquire(WithBackground(context.Background()), spec)
	require.NoError(t, err)

	started := time.Now()
	_, err = manager.Acquire(WithBackground(context.Background()), spec)
	var deferred *AdmissionDeferredError
	require.ErrorAs(t, err, &deferred)
	require.GreaterOrEqual(t, deferred.RetryAfter, time.Second)
	require.Less(t, time.Since(started), 50*time.Millisecond)

	first.Release()
	next, err := manager.Acquire(WithBackground(context.Background()), spec)
	require.NoError(t, err)
	next.Release()
}

func TestBackgroundAdmissionPreservesInteractiveReserve(t *testing.T) {
	config := testConfig(Limit{Concurrency: 2, PerTenant: 2})
	config.InteractiveReserve = 1
	manager := newManagerWithConfig(nil, config)
	spec := Spec{Kind: KindChat, Domain: "shared", TenantID: 100}

	background, err := manager.Acquire(WithBackground(context.Background()), spec)
	require.NoError(t, err)
	blockedCtx, cancel := context.WithTimeout(WithBackground(context.Background()), 50*time.Millisecond)
	defer cancel()
	_, err = manager.Acquire(blockedCtx, spec)
	require.ErrorIs(t, err, ErrAdmissionDeferred)

	interactive, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)
	interactive.Release()
	background.Release()
}

func TestLocalAdmissionEnforcesRPM(t *testing.T) {
	config := testConfig(Limit{Concurrency: 2, RPM: 1, PerTenant: 2})
	manager := newManagerWithConfig(nil, config)
	spec := Spec{Kind: KindChat, Domain: "rate-domain", TenantID: 100}
	first, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)
	first.Release()

	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = manager.Acquire(waitCtx, spec)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestSpecForModelIsStableScopedAndDoesNotExposeSecret(t *testing.T) {
	model := &types.Model{
		ID:       "model-id",
		TenantID: 100,
		Name:     "DeepSeek V4 Flash",
		Source:   types.ModelSourceRemote,
		Parameters: types.ModelParameters{
			Provider: "litellm",
			BaseURL:  "HTTPS://LITELLM.EXAMPLE/v1/?ignored=true",
			APIKey:   "secret-value-that-must-not-appear",
		},
	}
	first := SpecForModel(KindChat, model, "")
	second := SpecForModel(KindChat, model, "")
	require.Equal(t, first, second)
	require.Len(t, first.Domain, 32)
	require.NotContains(t, first.Domain, "secret")

	otherTenant := *model
	otherTenant.TenantID = 200
	third := SpecForModel(KindChat, &otherTenant, "")
	// Account quota domain is shared; tenant fairness is a separate key.
	require.Equal(t, first.Domain, third.Domain)
	require.NotEqual(t, first.TenantID, third.TenantID)

	otherSecret := *model
	otherSecret.Parameters.APIKey = "another-secret"
	require.Equal(t, first.Domain, SpecForModel(KindChat, &otherSecret, "").Domain)
	require.Equal(t, first.Domain, SpecForModel(KindEmbedding, &otherSecret, "").Domain)
	require.False(t, strings.Contains(first.Domain, model.Parameters.APIKey))
}

func TestFailClosedWhenRedisAdmissionBackendIsUnavailable(t *testing.T) {
	config := testConfig(Limit{Concurrency: 1})
	manager := newManagerWithConfig(unreachableRedisClient(t), config)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	lease, err := manager.Acquire(ctx, Spec{Kind: KindChat, Domain: "shared", TenantID: 1})
	require.Nil(t, lease)
	require.ErrorIs(t, err, ErrAdmissionBackendUnavailable)
	require.EqualValues(t, 1, manager.Snapshot().BackendErrors)
}

func TestRedisLeaseLosesFenceWhenAnyRequiredMemberDisappears(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not configured")
	}
	client := configuredRedisTestClient(address)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	config := testConfig(Limit{Concurrency: 2, PerTenant: 1})
	config.KeyPrefix = "test:model-admission:" + uuid.NewString() + ":"
	config.InteractiveReserve = 1
	config.LeaseTTL = 600 * time.Millisecond
	config.HeartbeatInterval = 40 * time.Millisecond
	manager := newManagerWithConfig(client, config)
	spec := Spec{Kind: KindChat, Domain: "partial-member", TenantID: 100}
	lease, err := manager.Acquire(WithBackground(context.Background()), spec)
	require.NoError(t, err)
	t.Cleanup(lease.Release)
	t.Cleanup(func() {
		_ = client.Del(
			context.Background(),
			lease.keys.total,
			lease.keys.background,
			lease.keys.tenant,
			lease.keys.rate,
		).Err()
	})
	require.EqualValues(t, 3, lease.expectedRenewals)
	require.NoError(t, client.ZRem(context.Background(), lease.keys.tenant, lease.token).Err())
	require.Eventually(t, func() bool {
		return errors.Is(context.Cause(lease.Context()), ErrAdmissionLeaseLost)
	}, time.Second, 10*time.Millisecond)
	require.ErrorIs(t, lease.FencingError(), ErrAdmissionLeaseLost)
}

func TestRedisHeartbeatRefreshesLeaseSetTTL(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not configured")
	}
	client := configuredRedisTestClient(address)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	config := testConfig(Limit{Concurrency: 2, PerTenant: 1})
	config.KeyPrefix = "test:model-admission:" + uuid.NewString() + ":"
	config.InteractiveReserve = 1
	config.LeaseTTL = 600 * time.Millisecond
	config.HeartbeatInterval = 40 * time.Millisecond
	manager := newManagerWithConfig(client, config)
	spec := Spec{Kind: KindChat, Domain: "long-running-call", TenantID: 100}
	lease, err := manager.Acquire(WithBackground(context.Background()), spec)
	require.NoError(t, err)
	t.Cleanup(lease.Release)
	t.Cleanup(func() {
		_ = client.Del(
			context.Background(),
			lease.keys.total,
			lease.keys.background,
			lease.keys.tenant,
			lease.keys.rate,
		).Err()
	})

	// Reproduce a lease-set key approaching expiry while its member remains
	// valid. A heartbeat must extend the key TTL as well as the member score.
	for _, key := range []string{lease.keys.total, lease.keys.background, lease.keys.tenant} {
		require.NoError(t, client.PExpire(context.Background(), key, 120*time.Millisecond).Err())
	}
	require.Eventually(t, func() bool {
		for _, key := range []string{lease.keys.total, lease.keys.background, lease.keys.tenant} {
			ttl, ttlErr := client.PTTL(context.Background(), key).Result()
			if ttlErr != nil || ttl < 500*time.Millisecond {
				return false
			}
		}
		return true
	}, time.Second, 10*time.Millisecond)

	time.Sleep(250 * time.Millisecond)
	require.NoError(t, lease.FencingError())
	for _, key := range []string{lease.keys.total, lease.keys.background, lease.keys.tenant} {
		score, scoreErr := client.ZScore(context.Background(), key, lease.token).Result()
		require.NoError(t, scoreErr)
		require.Greater(t, score, float64(0))
	}
}

func TestRedisAdmissionAcrossInstancesIsLiveAndTenantBounded(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not configured")
	}
	clients := make([]*redis.Client, 3)
	managers := make([]*Manager, 3)
	config := testConfig(Limit{Concurrency: 2, PerTenant: 1})
	config.KeyPrefix = "test:model-admission:" + uuid.NewString() + ":"
	config.BackgroundMaxWait = 0
	for index := range clients {
		clients[index] = configuredRedisTestClient(address)
		require.NoError(t, clients[index].Ping(context.Background()).Err())
		managers[index] = newManagerWithConfig(clients[index], config)
	}
	t.Cleanup(func() {
		for _, client := range clients {
			_ = client.Close()
		}
	})

	runCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var current atomic.Int64
	var maximum atomic.Int64
	var tenantMu sync.Mutex
	tenantCurrent := map[uint64]int{}
	errs := make(chan error, 30)
	var workers sync.WaitGroup
	for index := 0; index < 30; index++ {
		index := index
		workers.Add(1)
		go func() {
			defer workers.Done()
			tenantID := uint64(index%3 + 1)
			lease, err := managers[index%len(managers)].Acquire(
				WithBackground(runCtx),
				Spec{Kind: KindChat, Domain: "shared-provider", TenantID: tenantID},
			)
			if err != nil {
				errs <- err
				return
			}
			active := current.Add(1)
			for {
				observed := maximum.Load()
				if active <= observed || maximum.CompareAndSwap(observed, active) {
					break
				}
			}
			tenantMu.Lock()
			tenantCurrent[tenantID]++
			if tenantCurrent[tenantID] > 1 {
				errs <- errors.New("per-tenant admission limit exceeded")
			}
			tenantMu.Unlock()

			time.Sleep(12 * time.Millisecond)

			tenantMu.Lock()
			tenantCurrent[tenantID]--
			tenantMu.Unlock()
			current.Add(-1)
			lease.Release()
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.LessOrEqual(t, maximum.Load(), int64(2))
	require.EqualValues(t, 30, managers[0].Snapshot().Acquired+
		managers[1].Snapshot().Acquired+managers[2].Snapshot().Acquired)
}
