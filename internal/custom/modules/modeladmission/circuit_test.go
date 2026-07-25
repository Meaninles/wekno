package modeladmission

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func circuitTestConfig() Config {
	config := testConfig(Limit{Concurrency: 4, PerTenant: 4})
	config.CircuitEnabled = true
	config.CircuitThreshold = 2
	config.CircuitWindow = time.Second
	config.CircuitOpen = 50 * time.Millisecond
	config.CircuitProbeTTL = time.Second
	return config
}

func finishProviderCall(t *testing.T, manager *Manager, spec Spec, callErr error) {
	t.Helper()
	lease, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)
	lease.Finish(callErr)
	lease.Release()
}

func TestLocalCircuitOpensRejectsAndRecoversThroughOneProbe(t *testing.T) {
	manager := newManagerWithConfig(nil, circuitTestConfig())
	spec := Spec{Kind: KindChat, Domain: "local-circuit", TenantID: 1}

	finishProviderCall(t, manager, spec, errors.New("connection refused"))
	finishProviderCall(t, manager, spec, errors.New("connection refused"))

	_, err := manager.Acquire(context.Background(), spec)
	require.ErrorIs(t, err, ErrProviderCircuitOpen)
	retryAfter, ok := CircuitRetryAfter(err)
	require.True(t, ok)
	require.Positive(t, retryAfter)

	time.Sleep(60 * time.Millisecond)
	probe, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, probe.circuitProbe)

	_, err = manager.Acquire(context.Background(), spec)
	require.ErrorIs(t, err, ErrProviderCircuitOpen)

	probe.Finish(nil)
	probe.Release()
	healthy, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)
	require.False(t, healthy.circuitProbe)
	healthy.Finish(nil)
	healthy.Release()

	stats := manager.Snapshot()
	require.EqualValues(t, 1, stats.CircuitOpened)
	require.EqualValues(t, 2, stats.CircuitReject)
}

func TestCircuitIgnoresCallerErrorsAndCancellation(t *testing.T) {
	manager := newManagerWithConfig(nil, circuitTestConfig())
	spec := Spec{Kind: KindChat, Domain: "caller-error", TenantID: 1}

	for range 4 {
		finishProviderCall(t, manager, spec,
			&chat.HTTPStatusError{StatusCode: http.StatusBadRequest})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	lease, err := manager.Acquire(cancelled, spec)
	require.NoError(t, err)
	cancel()
	lease.Finish(context.Canceled)
	lease.Release()

	healthy, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)
	healthy.Finish(nil)
	healthy.Release()
	require.Zero(t, manager.Snapshot().CircuitOpened)
}

func TestCircuitFailureClassification(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{&chat.HTTPStatusError{StatusCode: http.StatusInternalServerError}, true},
		{&chat.HTTPStatusError{StatusCode: http.StatusTooManyRequests}, true},
		{&chat.HTTPStatusError{StatusCode: http.StatusUnauthorized}, true},
		{&chat.HTTPStatusError{StatusCode: http.StatusBadRequest}, false},
		{errors.New("send request: no route to host"), true},
		{errors.New("API request failed with status 503"), true},
		{errors.New("invalid JSON in tool arguments"), false},
		{context.Canceled, false},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, isCircuitFailure(tt.err), tt.err)
	}
}

func TestDefaultCircuitWindowCoversSerializedSlowProviderCalls(t *testing.T) {
	t.Setenv("CUSTOM_MODEL_CIRCUIT_WINDOW_SECONDS", "")
	t.Setenv("CUSTOM_MODEL_CIRCUIT_OPEN_SECONDS", "")

	config := ConfigFromEnv()
	require.Equal(t, 10*time.Minute, config.CircuitWindow)
	require.Equal(t, time.Minute, config.CircuitOpen)
}

func TestSerialProviderFailuresAccumulateInsideCircuitWindow(t *testing.T) {
	config := circuitTestConfig()
	config.CircuitThreshold = 3
	config.CircuitWindow = 250 * time.Millisecond
	config.CircuitOpen = time.Second
	config.Limits[KindVLM] = Limit{Concurrency: 1, PerTenant: 1}
	manager := newManagerWithConfig(nil, config)
	spec := Spec{Kind: KindVLM, Domain: "serial-slow-provider", TenantID: 1}

	// Scale a multi-minute provider timeout down to milliseconds. Each call
	// finishes far enough apart that the former one-minute production window
	// would reset before the third serialized failure.
	for index := 0; index < 3; index++ {
		finishProviderCall(t, manager, spec, errors.New("i/o timeout"))
		if index < 2 {
			time.Sleep(80 * time.Millisecond)
		}
	}

	_, err := manager.Acquire(context.Background(), spec)
	require.ErrorIs(t, err, ErrProviderCircuitOpen)
	require.EqualValues(t, 1, manager.Snapshot().CircuitOpened)
}

func TestRedisCircuitIsSharedAndAllowsOneHalfOpenProbe(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not configured")
	}
	password := os.Getenv("WEKNORA_TEST_REDIS_PASSWORD")
	newClient := func() *redis.Client {
		return redis.NewClient(&redis.Options{
			Addr:       address,
			Password:   password,
			MaxRetries: -1,
		})
	}
	firstClient := newClient()
	secondClient := newClient()
	t.Cleanup(func() {
		_ = firstClient.Close()
		_ = secondClient.Close()
	})
	require.NoError(t, firstClient.Ping(context.Background()).Err())

	config := circuitTestConfig()
	config.KeyPrefix = "test:model-admission:" + uuid.NewString() + ":"
	config.CircuitThreshold = 1
	first := newManagerWithConfig(firstClient, config)
	second := newManagerWithConfig(secondClient, config)
	spec := Spec{Kind: KindChat, Domain: "redis-shared-circuit", TenantID: 1}
	keys := first.keys(spec)
	t.Cleanup(func() {
		_ = firstClient.Del(context.Background(),
			keys.total, keys.background, keys.tenant, keys.rate,
			keys.circuit, keys.probe).Err()
	})

	finishProviderCall(t, first, spec, errors.New("connection refused"))
	_, err := second.Acquire(context.Background(), spec)
	require.ErrorIs(t, err, ErrProviderCircuitOpen)

	time.Sleep(60 * time.Millisecond)
	probe, err := second.Acquire(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, probe.circuitProbe)
	_, err = first.Acquire(context.Background(), spec)
	require.ErrorIs(t, err, ErrProviderCircuitOpen)

	probe.Finish(nil)
	probe.Release()
	healthy, err := first.Acquire(context.Background(), spec)
	require.NoError(t, err)
	healthy.Finish(nil)
	healthy.Release()
}

func TestRedisCircuitAccumulatesSerializedFailuresAcrossReplicas(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not configured")
	}
	newClient := func() *redis.Client {
		return redis.NewClient(&redis.Options{
			Addr:       address,
			Password:   os.Getenv("WEKNORA_TEST_REDIS_PASSWORD"),
			MaxRetries: -1,
		})
	}
	firstClient := newClient()
	secondClient := newClient()
	t.Cleanup(func() {
		_ = firstClient.Close()
		_ = secondClient.Close()
	})
	require.NoError(t, firstClient.Ping(context.Background()).Err())

	config := circuitTestConfig()
	config.KeyPrefix = "test:model-admission:" + uuid.NewString() + ":"
	config.CircuitThreshold = 3
	config.CircuitWindow = 500 * time.Millisecond
	config.CircuitOpen = time.Second
	config.Limits[KindVLM] = Limit{Concurrency: 2, PerTenant: 2}
	first := newManagerWithConfig(firstClient, config)
	second := newManagerWithConfig(secondClient, config)
	spec := Spec{Kind: KindVLM, Domain: "redis-serial-vlm-outage", TenantID: 1}
	keys := first.keys(spec)
	t.Cleanup(func() {
		_ = firstClient.Del(context.Background(),
			keys.total, keys.background, keys.tenant, keys.rate,
			keys.circuit, keys.probe).Err()
	})

	finishProviderCall(t, first, spec, errors.New("i/o timeout"))
	time.Sleep(100 * time.Millisecond)
	finishProviderCall(t, second, spec, errors.New("connection refused"))
	time.Sleep(100 * time.Millisecond)
	finishProviderCall(t, first, spec, errors.New("context deadline exceeded"))

	_, err := second.Acquire(context.Background(), spec)
	require.ErrorIs(t, err, ErrProviderCircuitOpen)
}

func TestRedisCircuitReclaimsProbeWhoseOwnerLeaseExpired(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not configured")
	}
	newClient := func() *redis.Client {
		return redis.NewClient(&redis.Options{
			Addr:       address,
			Password:   os.Getenv("WEKNORA_TEST_REDIS_PASSWORD"),
			MaxRetries: -1,
		})
	}
	firstClient := newClient()
	secondClient := newClient()
	t.Cleanup(func() {
		_ = firstClient.Close()
		_ = secondClient.Close()
	})
	require.NoError(t, firstClient.Ping(context.Background()).Err())

	config := circuitTestConfig()
	config.KeyPrefix = "test:model-admission:" + uuid.NewString() + ":"
	config.CircuitThreshold = 1
	config.CircuitOpen = 50 * time.Millisecond
	config.CircuitProbeTTL = 5 * time.Second
	first := newManagerWithConfig(firstClient, config)
	second := newManagerWithConfig(secondClient, config)
	spec := Spec{Kind: KindChat, Domain: "redis-orphaned-probe", TenantID: 1}
	keys := first.keys(spec)
	t.Cleanup(func() {
		_ = firstClient.Del(context.Background(),
			keys.total, keys.background, keys.tenant, keys.rate,
			keys.circuit, keys.probe).Err()
	})

	finishProviderCall(t, first, spec, errors.New("connection refused"))
	time.Sleep(60 * time.Millisecond)
	orphanedProbe, err := first.Acquire(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, orphanedProbe.circuitProbe)

	_, err = second.Acquire(context.Background(), spec)
	require.ErrorIs(t, err, ErrProviderCircuitOpen)

	// Model a hard-killed Pod: no Release/Finish callback runs, but its
	// renewable total admission lease is gone. The next replica must reclaim
	// the probe immediately rather than waiting the five-second probe TTL.
	require.NoError(t, firstClient.ZRem(context.Background(), keys.total, orphanedProbe.token).Err())
	replacement, err := second.Acquire(context.Background(), spec)
	require.NoError(t, err)
	require.True(t, replacement.circuitProbe)
	replacement.Finish(nil)
	replacement.Release()

	// Do not call orphanedProbe.Release before the assertion above: that is
	// precisely the callback a killed process cannot execute.
	orphanedProbe.Release()
}
