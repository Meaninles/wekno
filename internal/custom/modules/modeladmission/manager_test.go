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

func TestBackgroundAdmissionWaitsForCapacityWithoutBusinessRetry(t *testing.T) {
	config := testConfig(Limit{Concurrency: 1, PerTenant: 1})
	config.BackgroundMaxWait = 0
	manager := newManagerWithConfig(nil, config)
	spec := Spec{Kind: KindChat, Domain: "durable-background", TenantID: 100}
	first, err := manager.Acquire(WithBackground(context.Background()), spec)
	require.NoError(t, err)

	released := make(chan struct{})
	go func() {
		time.Sleep(40 * time.Millisecond)
		first.Release()
		close(released)
	}()
	started := time.Now()
	next, err := manager.Acquire(WithBackground(context.Background()), spec)
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(started), 35*time.Millisecond)
	<-released
	next.Release()
}

func TestFiniteBackgroundWaitYieldsWithoutFailureBudget(t *testing.T) {
	config := testConfig(Limit{Concurrency: 1, PerTenant: 1})
	config.BackgroundMaxWait = 35 * time.Millisecond
	manager := newManagerWithConfig(nil, config)
	spec := Spec{Kind: KindChat, Domain: "finite-background-wait", TenantID: 100}
	first, err := manager.Acquire(WithBackground(context.Background()), spec)
	require.NoError(t, err)
	defer first.Release()

	started := time.Now()
	_, err = manager.Acquire(WithBackground(context.Background()), spec)
	var deferred *AdmissionDeferredError
	require.ErrorAs(t, err, &deferred)
	require.ErrorIs(t, err, ErrAdmissionDeferred)
	require.GreaterOrEqual(t, time.Since(started), 30*time.Millisecond)
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
	require.ErrorIs(t, err, context.DeadlineExceeded)

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

func TestWorkWindowIsGlobalWorkConservingAndBounded(t *testing.T) {
	config := testConfig(Limit{Concurrency: 3, Background: 3, PerTenant: 3})
	config.WorkWindowEnabled = true
	config.WorkPrefetchFactor = 2
	config.DerivativeWeight = 2
	config.WikiWeight = 1
	manager := newManagerWithConfig(nil, config)
	policy := schedulerPolicyFromConfig(config)
	require.Equal(t, 6, WorkWindow(3, policy))
	require.Equal(t, 4, LaneWorkWindow(3, WorkLaneDerivative, policy))
	require.Equal(t, 2, LaneWorkWindow(3, WorkLaneWiki, policy))

	leases := make([]*WorkLease, 0, 6)
	for index := 0; index < 6; index++ {
		lease, err := manager.acquireWorkWindow(
			context.Background(), "shared-pool", WorkLaneDerivative, 3, policy,
		)
		require.NoError(t, err, "derivative must borrow Wiki's idle share")
		leases = append(leases, lease)
	}
	_, err := manager.acquireWorkWindow(
		context.Background(), "shared-pool", WorkLaneDerivative, 3, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred)
	require.EqualValues(t, 6, manager.Snapshot().WorkActive)
	for _, lease := range leases {
		lease.Release()
	}
	require.EqualValues(t, 0, manager.Snapshot().WorkActive)
}

func TestWikiStageSharesAreDerivedAndNeverRequireOperatorTuning(t *testing.T) {
	tests := []struct {
		total      int
		wantMap    int
		wantCommit int
	}{
		{total: 0, wantMap: 0, wantCommit: 0},
		{total: 1, wantMap: 1, wantCommit: 1},
		{total: 2, wantMap: 1, wantCommit: 1},
		{total: 3, wantMap: 2, wantCommit: 1},
		{total: 4, wantMap: 3, wantCommit: 1},
		{total: 6, wantMap: 4, wantCommit: 2},
		{total: 9, wantMap: 6, wantCommit: 3},
	}
	for _, test := range tests {
		wikiMap, wikiCommit := WikiStageShares(test.total)
		require.Equal(t, test.wantMap, wikiMap, "total=%d", test.total)
		require.Equal(t, test.wantCommit, wikiCommit, "total=%d", test.total)
	}
}

func TestWorkWindowReservesWikiCommitProgressAfterMapBorrowing(t *testing.T) {
	config := testConfig(Limit{Concurrency: 3, Background: 3, PerTenant: 3})
	config.WorkWindowEnabled = true
	manager := newManagerWithConfig(nil, config)
	policy := schedulerPolicyFromConfig(config)

	maps := make([]*WorkLease, 0, 6)
	for index := 0; index < 6; index++ {
		lease, err := manager.acquireWorkWindow(
			context.Background(), "wiki-stage-pool", WorkLaneWikiMap, 3, policy,
		)
		require.NoError(t, err, "Map must borrow idle commit capacity")
		maps = append(maps, lease)
	}

	_, err := manager.acquireWorkWindow(
		context.Background(), "wiki-stage-pool", WorkLaneWikiCommit, 3, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred)
	maps[0].Release()

	_, err = manager.acquireWorkWindow(
		context.Background(), "wiki-stage-pool", WorkLaneWikiMap, 3, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred,
		"Map may not reclaim a released slot while commit is waiting")
	commit, err := manager.acquireWorkWindow(
		context.Background(), "wiki-stage-pool", WorkLaneWikiCommit, 3, policy,
	)
	require.NoError(t, err)
	commit.Release()
	for _, lease := range maps[1:] {
		lease.Release()
	}
}

func TestProviderWindowReservesWikiCommitProgressAfterMapBorrowing(t *testing.T) {
	limit := Limit{Concurrency: 3, Background: 3, PerTenant: 3}
	config := testConfig(limit)
	manager := newManagerWithConfig(nil, config)
	policy := schedulerPolicyFromConfig(config)
	spec := Spec{Kind: KindDerivative, Domain: "wiki-provider-stage", TenantID: 7}

	for index := 0; index < 3; index++ {
		acquired, _ := manager.tryAcquireLocal(spec, limit, true, WorkLaneWikiMap, policy)
		require.True(t, acquired, "Map must borrow idle commit provider capacity")
	}
	acquired, _ := manager.tryAcquireLocal(spec, limit, true, WorkLaneWikiCommit, policy)
	require.False(t, acquired)
	manager.releaseLocal(spec, true, WorkLaneWikiMap)

	acquired, _ = manager.tryAcquireLocal(spec, limit, true, WorkLaneWikiMap, policy)
	require.False(t, acquired, "Map may not reclaim a provider slot while commit waits")
	acquired, _ = manager.tryAcquireLocal(spec, limit, true, WorkLaneWikiCommit, policy)
	require.True(t, acquired)

	manager.releaseLocal(spec, true, WorkLaneWikiCommit)
	manager.releaseLocal(spec, true, WorkLaneWikiMap)
	manager.releaseLocal(spec, true, WorkLaneWikiMap)
}

func TestWikiStagesAreSubdividedInsideContendedTopLevelShare(t *testing.T) {
	config := testConfig(Limit{Concurrency: 3, Background: 3, PerTenant: 3})
	config.WorkWindowEnabled = true
	manager := newManagerWithConfig(nil, config)
	policy := schedulerPolicyFromConfig(config)

	derivative := make([]*WorkLease, 0, 4)
	for index := 0; index < 4; index++ {
		lease, err := manager.acquireWorkWindow(
			context.Background(), "nested-work", WorkLaneDerivative, 3, policy,
		)
		require.NoError(t, err)
		derivative = append(derivative, lease)
	}
	maps := make([]*WorkLease, 0, 2)
	for index := 0; index < 2; index++ {
		lease, err := manager.acquireWorkWindow(
			context.Background(), "nested-work", WorkLaneWikiMap, 3, policy,
		)
		require.NoError(t, err)
		maps = append(maps, lease)
	}
	_, err := manager.acquireWorkWindow(
		context.Background(), "nested-work", WorkLaneDerivative, 3, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred, "publish derivative demand")
	_, err = manager.acquireWorkWindow(
		context.Background(), "nested-work", WorkLaneWikiCommit, 3, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred, "publish commit demand")

	maps[0].Release()
	_, err = manager.acquireWorkWindow(
		context.Background(), "nested-work", WorkLaneWikiMap, 3, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred,
		"Map must not reclaim both protected Wiki slots while commit waits")
	commit, err := manager.acquireWorkWindow(
		context.Background(), "nested-work", WorkLaneWikiCommit, 3, policy,
	)
	require.NoError(t, err)
	commit.Release()
	maps[1].Release()
	for _, lease := range derivative {
		lease.Release()
	}

	provider := newManagerWithConfig(nil, config)
	limit := Limit{Concurrency: 3, Background: 3, PerTenant: 3}
	spec := Spec{Kind: KindDerivative, Domain: "nested-provider", TenantID: 7}
	for index := 0; index < 2; index++ {
		acquired, _ := provider.tryAcquireLocal(spec, limit, true, WorkLaneDerivative, policy)
		require.True(t, acquired)
	}
	acquired, _ := provider.tryAcquireLocal(spec, limit, true, WorkLaneWikiMap, policy)
	require.True(t, acquired)
	acquired, _ = provider.tryAcquireLocal(spec, limit, true, WorkLaneDerivative, policy)
	require.False(t, acquired, "publish derivative provider demand")
	acquired, _ = provider.tryAcquireLocal(spec, limit, true, WorkLaneWikiCommit, policy)
	require.False(t, acquired, "publish commit provider demand")
	provider.releaseLocal(spec, true, WorkLaneWikiMap)
	acquired, _ = provider.tryAcquireLocal(spec, limit, true, WorkLaneWikiMap, policy)
	require.False(t, acquired,
		"the single protected Wiki provider slot must hand off to waiting commit")
	acquired, _ = provider.tryAcquireLocal(spec, limit, true, WorkLaneWikiCommit, policy)
	require.True(t, acquired)
	provider.releaseLocal(spec, true, WorkLaneWikiCommit)
	provider.releaseLocal(spec, true, WorkLaneDerivative)
	provider.releaseLocal(spec, true, WorkLaneDerivative)
}

func TestSingleSlotAutomaticallyHandsOffFromWikiMapToWaitingCommit(t *testing.T) {
	config := testConfig(Limit{Concurrency: 1, Background: 1, PerTenant: 1})
	config.WorkWindowEnabled = true
	config.WorkPrefetchFactor = 1
	manager := newManagerWithConfig(nil, config)
	policy := schedulerPolicyFromConfig(config)

	mapWork, err := manager.acquireWorkWindow(
		context.Background(), "single-work", WorkLaneWikiMap, 1, policy,
	)
	require.NoError(t, err)
	_, err = manager.acquireWorkWindow(
		context.Background(), "single-work", WorkLaneWikiCommit, 1, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred)
	mapWork.Release()
	_, err = manager.acquireWorkWindow(
		context.Background(), "single-work", WorkLaneWikiMap, 1, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred)
	commitWork, err := manager.acquireWorkWindow(
		context.Background(), "single-work", WorkLaneWikiCommit, 1, policy,
	)
	require.NoError(t, err)
	commitWork.Release()

	limit := Limit{Concurrency: 1, Background: 1, PerTenant: 1}
	spec := Spec{Kind: KindDerivative, Domain: "single-provider", TenantID: 7}
	acquired, _ := manager.tryAcquireLocal(spec, limit, true, WorkLaneWikiMap, policy)
	require.True(t, acquired)
	acquired, _ = manager.tryAcquireLocal(spec, limit, true, WorkLaneWikiCommit, policy)
	require.False(t, acquired)
	manager.releaseLocal(spec, true, WorkLaneWikiMap)
	acquired, _ = manager.tryAcquireLocal(spec, limit, true, WorkLaneWikiMap, policy)
	require.False(t, acquired)
	acquired, _ = manager.tryAcquireLocal(spec, limit, true, WorkLaneWikiCommit, policy)
	require.True(t, acquired)
	manager.releaseLocal(spec, true, WorkLaneWikiCommit)
}

func TestWorkWindowRestoresWeightedShareWhenOtherLaneWaits(t *testing.T) {
	config := testConfig(Limit{Concurrency: 3, Background: 3, PerTenant: 3})
	config.WorkWindowEnabled = true
	manager := newManagerWithConfig(nil, config)
	policy := schedulerPolicyFromConfig(config)

	derivative := make([]*WorkLease, 0, 4)
	for index := 0; index < 4; index++ {
		lease, err := manager.acquireWorkWindow(context.Background(), "fair-pool", WorkLaneDerivative, 3, policy)
		require.NoError(t, err)
		derivative = append(derivative, lease)
	}
	wiki := make([]*WorkLease, 0, 2)
	for index := 0; index < 2; index++ {
		lease, err := manager.acquireWorkWindow(context.Background(), "fair-pool", WorkLaneWiki, 3, policy)
		require.NoError(t, err)
		wiki = append(wiki, lease)
	}
	_, err := manager.acquireWorkWindow(context.Background(), "fair-pool", WorkLaneWiki, 3, policy)
	require.ErrorIs(t, err, ErrAdmissionDeferred, "a denied Wiki request must publish a waiter marker")
	wiki[0].Release()
	_, err = manager.acquireWorkWindow(context.Background(), "fair-pool", WorkLaneDerivative, 3, policy)
	require.ErrorIs(t, err, ErrAdmissionDeferred, "derivative may not steal Wiki's reserved share while Wiki waits")
	replacement, err := manager.acquireWorkWindow(context.Background(), "fair-pool", WorkLaneWiki, 3, policy)
	require.NoError(t, err)
	replacement.Release()
	wiki[1].Release()
	for _, lease := range derivative {
		lease.Release()
	}
}

func TestWorkWindowWaitingReservationCoversDeferredRetry(t *testing.T) {
	config := testConfig(Limit{Concurrency: 3, Background: 3, PerTenant: 3})
	config.WorkWindowEnabled = true
	manager := newManagerWithConfig(nil, config)
	policy := schedulerPolicyFromConfig(config)

	wiki := make([]*WorkLease, 0, 6)
	for index := 0; index < 6; index++ {
		lease, err := manager.acquireWorkWindow(
			context.Background(), "waiter-ttl-pool", WorkLaneWiki, 3, policy,
		)
		require.NoError(t, err)
		wiki = append(wiki, lease)
	}

	_, err := manager.acquireWorkWindow(
		context.Background(), "waiter-ttl-pool", WorkLaneDerivative, 3, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred)
	var deferred *AdmissionDeferredError
	require.ErrorAs(t, err, &deferred)
	require.Equal(t, workAdmissionRetryAfter, deferred.RetryAfter)

	manager.localMu.Lock()
	waitRemaining := time.Until(
		manager.localWork["waiter-ttl-pool"].waitUntil[WorkLaneDerivative.index()],
	)
	manager.localMu.Unlock()
	require.GreaterOrEqual(t, waitRemaining, workAdmissionRetryAfter,
		"the protected-share waiter must remain visible through the next durable retry")

	wiki[0].Release()
	_, err = manager.acquireWorkWindow(
		context.Background(), "waiter-ttl-pool", WorkLaneWiki, 3, policy,
	)
	require.ErrorIs(t, err, ErrAdmissionDeferred,
		"the borrowing lane must not reclaim a released slot while derivative is waiting")
	replacement, err := manager.acquireWorkWindow(
		context.Background(), "waiter-ttl-pool", WorkLaneDerivative, 3, policy,
	)
	require.NoError(t, err)
	replacement.Release()
	for _, lease := range wiki[1:] {
		lease.Release()
	}
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

func TestRedisAdmissionDoesNotEraseRPMWindowWhileCleaningLeases(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("WEKNORA_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("WEKNORA_TEST_REDIS_ADDR is not configured")
	}
	client := configuredRedisTestClient(address)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	config := testConfig(Limit{Concurrency: 2, RPM: 1, PerTenant: 2})
	config.KeyPrefix = "test:model-admission:" + uuid.NewString() + ":"
	manager := newManagerWithConfig(client, config)
	spec := Spec{Kind: KindChat, Domain: "rpm-window", TenantID: 100}
	first, err := manager.Acquire(context.Background(), spec)
	require.NoError(t, err)
	first.Release()

	waitCtx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = manager.Acquire(waitCtx, spec)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.EqualValues(t, 1, client.ZCard(context.Background(), manager.keys(spec).rate).Val())
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
