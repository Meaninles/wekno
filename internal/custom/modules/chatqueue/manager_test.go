package chatqueue

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	sessionhandler "github.com/Tencent/WeKnora/internal/handler/session"
	"github.com/Tencent/WeKnora/internal/types"
)

type testSettings struct{}

func (testSettings) GetInt(_ context.Context, key, _ string, def int64) int64 {
	switch key {
	case "chat.queue.default_max_waiting":
		return 10
	case "chat.queue.max_waiting_per_user":
		return 3
	default:
		return def
	}
}
func (testSettings) GetString(_ context.Context, _, _, def string) string  { return def }
func (testSettings) GetBool(_ context.Context, _, _ string, def bool) bool { return def }
func (testSettings) GetStringList(_ context.Context, _, _ string, def []string) []string {
	return def
}
func (testSettings) List(context.Context) ([]*types.SystemSetting, error) { return nil, nil }
func (testSettings) Get(context.Context, string) (*types.SystemSetting, error) {
	return nil, nil
}
func (testSettings) Update(context.Context, string, any) (*types.SystemSetting, error) {
	return nil, nil
}
func (testSettings) Reset(context.Context, string) error  { return nil }
func (testSettings) SubscribeRedis(context.Context) error { return nil }

func newLocalTestManager() *Manager {
	return &Manager{
		keyPrefix:        "test:",
		maxWait:          3 * time.Second,
		localPools:       make(map[string]*localPool),
		localUserWaiting: make(map[string]map[string]int64),
		poolCache:        make(map[string]cachedPool),
	}
}

func TestResourcePoolTotalIsTheOnlyChatExecutionLimit(t *testing.T) {
	manager := &Manager{settings: testSettings{}}
	legacy := 1
	policy := manager.policyWithPool(context.Background(), &modeladmission.ResourcePool{
		MaxInflight: 4, ChatMaxConcurrent: &legacy,
	})
	if policy.MaxConcurrent != 4 {
		t.Fatalf("MaxConcurrent = %d, want resource-pool total 4", policy.MaxConcurrent)
	}
}

func newLocalTestTicket(manager *Manager, token, principal string) *Ticket {
	return &Ticket{
		manager:       manager,
		token:         token,
		principalHash: principal,
		poolID:        "pool-a",
		modelID:       "model-a",
		local:         true,
		queuedAt:      time.Now().UTC(),
		stopHeartbeat: make(chan struct{}),
	}
}

func TestLocalQueueEnforcesFIFOAndReleasesSlot(t *testing.T) {
	manager := newLocalTestManager()
	policy := queuePolicy{Enabled: true, MaxConcurrent: 1, MaxWaiting: 10, MaxPerUser: 3}
	first := newLocalTestTicket(manager, "first", "user-a")
	second := newLocalTestTicket(manager, "second", "user-b")
	third := newLocalTestTicket(manager, "third", "user-c")

	got, err := manager.admitLocal(first, policy)
	if err != nil || got.code != 1 {
		t.Fatalf("first admission = %#v, err=%v", got, err)
	}
	got, _ = manager.admitLocal(second, policy)
	if got.code != 0 || got.position != 1 {
		t.Fatalf("second admission = %#v, want first waiter", got)
	}
	second.queued = true
	got, _ = manager.admitLocal(third, policy)
	if got.code != 0 || got.position != 2 {
		t.Fatalf("third admission = %#v, want second waiter", got)
	}
	third.queued = true

	if promoted, _ := manager.promoteLocal(third, policy); promoted.code != 0 || promoted.position != 2 {
		t.Fatalf("third jumped FIFO queue: %#v", promoted)
	}
	first.admitted.Store(true)
	first.Release(context.Background())
	if promoted, _ := manager.promoteLocal(second, policy); promoted.code != 1 {
		t.Fatalf("second was not promoted after release: %#v", promoted)
	}
	if promoted, _ := manager.promoteLocal(third, policy); promoted.code != 0 || promoted.position != 1 {
		t.Fatalf("third should remain queued while second is active: %#v", promoted)
	}
	second.admitted.Store(true)
	second.Release(context.Background())
	if promoted, _ := manager.promoteLocal(third, policy); promoted.code != 1 {
		t.Fatalf("third was not promoted after second release: %#v", promoted)
	}
	third.admitted.Store(true)
	third.Release(context.Background())
}

func TestLocalQueueDistinguishesUserAndPoolLimits(t *testing.T) {
	manager := newLocalTestManager()
	policy := queuePolicy{Enabled: true, MaxConcurrent: 1, MaxWaiting: 2, MaxPerUser: 1}
	active := newLocalTestTicket(manager, "active", "owner")
	userFirst := newLocalTestTicket(manager, "user-first", "same-user")
	userSecond := newLocalTestTicket(manager, "user-second", "same-user")
	other := newLocalTestTicket(manager, "other", "other-user")
	overflow := newLocalTestTicket(manager, "overflow", "third-user")

	if got, _ := manager.admitLocal(active, policy); got.code != 1 {
		t.Fatalf("active admission = %#v", got)
	}
	if got, _ := manager.admitLocal(userFirst, policy); got.code != 0 {
		t.Fatalf("first user waiter = %#v", got)
	}
	if got, _ := manager.admitLocal(userSecond, policy); got.code != -2 || got.userWaiting != 1 {
		t.Fatalf("per-user rejection = %#v, want code -2", got)
	}
	if got, _ := manager.admitLocal(other, policy); got.code != 0 {
		t.Fatalf("other user waiter = %#v", got)
	}
	if got, _ := manager.admitLocal(overflow, policy); got.code != -1 || got.waiting != 2 {
		t.Fatalf("pool-full rejection = %#v, want code -1", got)
	}

	userFirst.Cancel(context.Background())
	if got, _ := manager.admitLocal(userSecond, policy); got.code != 0 {
		t.Fatalf("cancel did not free per-user waiting capacity: %#v", got)
	}
	active.Cancel(context.Background())
	other.Cancel(context.Background())
	userSecond.Cancel(context.Background())
}

func TestTicketWaitPublishesWaitingThenAdmitted(t *testing.T) {
	manager := newLocalTestManager()
	manager.settings = testSettings{}
	policy := queuePolicy{Enabled: true, MaxConcurrent: 1, MaxWaiting: 10, MaxPerUser: 3}
	active := newLocalTestTicket(manager, "active", "owner")
	waiter := newLocalTestTicket(manager, "waiter", "user")
	if got, _ := manager.admitLocal(active, policy); got.code != 1 {
		t.Fatalf("active admission = %#v", got)
	}
	result, _ := manager.admitLocal(waiter, policy)
	waiter.queued = true
	waiter.lastSnapshot = waiter.snapshot(
		"waiting", result.position, result.active, result.waiting, policy,
	)

	updates := make(chan sessionhandler.ChatQueueSnapshot, 4)
	done := make(chan error, 1)
	go func() {
		done <- waiter.Wait(context.Background(), func(snapshot sessionhandler.ChatQueueSnapshot) {
			updates <- snapshot
		})
	}()

	firstUpdate := <-updates
	if firstUpdate.State != "waiting" || firstUpdate.Position != 1 {
		t.Fatalf("first update = %#v", firstUpdate)
	}
	active.admitted.Store(true)
	active.Release(context.Background())

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not promoted")
	}
	var admitted sessionhandler.ChatQueueSnapshot
	for len(updates) > 0 {
		admitted = <-updates
	}
	if admitted.State != "admitted" {
		t.Fatalf("final update = %#v, want admitted", admitted)
	}
	waiter.Release(context.Background())
}

// Compile-time guard for the native hook boundary.
var _ sessionhandler.ChatQueueTicket = (*Ticket)(nil)
