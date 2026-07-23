package router

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/taskretry"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
)

func TestSyncTaskExecutorPublishesRetryMetadataForEveryAttempt(t *testing.T) {
	executor := NewSyncTaskExecutor()
	executor.retryDelay = func(int) time.Duration { return 0 }

	type observation struct {
		retry int
		max   int
		ok    bool
	}
	var (
		mu   sync.Mutex
		seen []observation
	)
	done := make(chan struct{})
	executor.RegisterHandler("test:retry-metadata", func(ctx context.Context, _ *asynq.Task) error {
		retry, maxRetry, ok := taskretry.Metadata(ctx)
		mu.Lock()
		seen = append(seen, observation{retry: retry, max: maxRetry, ok: ok})
		count := len(seen)
		mu.Unlock()
		if count == 3 {
			close(done)
		}
		return errors.New("permanent failure")
	})

	if _, err := executor.Enqueue(
		asynq.NewTask("test:retry-metadata", nil),
		asynq.MaxRetry(2),
	); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lite retry attempts")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []observation{
		{retry: 0, max: 2, ok: true},
		{retry: 1, max: 2, ok: true},
		{retry: 2, max: 2, ok: true},
	}
	if len(seen) != len(want) {
		t.Fatalf("observations = %#v, want %#v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("observation[%d] = %#v, want %#v", i, seen[i], want[i])
		}
	}
}

func TestSyncTaskExecutorHonorsStableTaskIDAndRetention(t *testing.T) {
	executor := NewSyncTaskExecutor()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	executor.RegisterHandler("test:stable-id", func(context.Context, *asynq.Task) error {
		close(started)
		<-release
		close(done)
		return nil
	})
	opts := []asynq.Option{
		asynq.TaskID("stable-child"),
		asynq.MaxRetry(0),
		asynq.Retention(time.Minute),
	}
	if _, err := executor.Enqueue(asynq.NewTask("test:stable-id", nil), opts...); err != nil {
		t.Fatalf("first Enqueue() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stable-ID task did not start")
	}
	if _, err := executor.Enqueue(asynq.NewTask("test:stable-id", nil), opts...); !errors.Is(err, asynq.ErrTaskIDConflict) {
		t.Fatalf("active duplicate error = %v, want ErrTaskIDConflict", err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stable-ID task did not finish")
	}
	waitForSyncRecordToDisappear(t, executor, "stable-child")
	if _, err := executor.Enqueue(asynq.NewTask("test:stable-id", nil), opts...); !errors.Is(err, asynq.ErrTaskIDConflict) {
		t.Fatalf("retained duplicate error = %v, want ErrTaskIDConflict", err)
	}
}

func TestSyncTaskExecutorReleasesUniqueLeaseImmediatelyAfterSuccess(t *testing.T) {
	executor := NewSyncTaskExecutor()
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	var runs atomic.Int32
	executor.RegisterHandler("test:unique", func(context.Context, *asynq.Task) error {
		switch runs.Add(1) {
		case 1:
			close(started)
			<-release
			close(firstDone)
		case 2:
			close(secondDone)
		}
		return nil
	})
	opts := []asynq.Option{asynq.MaxRetry(0), asynq.Unique(time.Minute)}
	newTask := func() *asynq.Task { return asynq.NewTask("test:unique", []byte(`{"same":true}`)) }
	if _, err := executor.Enqueue(newTask(), opts...); err != nil {
		t.Fatalf("first Enqueue() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("unique task did not start")
	}
	if _, err := executor.Enqueue(newTask(), opts...); !errors.Is(err, asynq.ErrDuplicateTask) {
		t.Fatalf("active unique duplicate error = %v, want ErrDuplicateTask", err)
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("unique task did not finish")
	}
	waitForSyncTaskCount(t, executor, 0)
	if _, err := executor.Enqueue(newTask(), opts...); err != nil {
		t.Fatalf("Enqueue() after successful Unique task error = %v", err)
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("task did not run after Unique TTL expired")
	}
}

func TestComposeSyncTaskOptionsLastSchedulingOptionWins(t *testing.T) {
	base := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	options, err := composeSyncTaskOptions(base, []asynq.Option{
		asynq.ProcessAt(base.Add(time.Hour)),
		asynq.ProcessIn(2 * time.Hour),
		asynq.ProcessAt(base.Add(3 * time.Hour)),
		asynq.Queue("low"),
		asynq.Queue("critical"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := base.Add(3 * time.Hour); !options.processAt.Equal(want) {
		t.Fatalf("processAt = %v, want %v", options.processAt, want)
	}
	if options.queue != "critical" {
		t.Fatalf("queue = %q, want critical", options.queue)
	}
}

func TestSyncAttemptDeadlineUsesEarliestAndDefault(t *testing.T) {
	base := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	if got, want := syncAttemptDeadline(base, syncTaskOptions{}), base.Add(30*time.Minute); !got.Equal(want) {
		t.Fatalf("default deadline = %v, want %v", got, want)
	}
	options := syncTaskOptions{timeout: 10 * time.Minute, deadline: base.Add(4 * time.Minute)}
	if got, want := syncAttemptDeadline(base, options), base.Add(4*time.Minute); !got.Equal(want) {
		t.Fatalf("earliest deadline = %v, want %v", got, want)
	}
	options.deadline = base.Add(20 * time.Minute)
	if got, want := syncAttemptDeadline(base, options), base.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("timeout deadline = %v, want %v", got, want)
	}
}

func TestSyncTaskExecutorTimeoutRetriesAndPublishesQueue(t *testing.T) {
	executor := NewSyncTaskExecutor()
	executor.retryDelay = func(int) time.Duration { return 0 }
	var attempts atomic.Int32
	done := make(chan struct{})
	executor.RegisterHandler("test:timeout", func(ctx context.Context, _ *asynq.Task) error {
		defer func() {
			if attempts.Add(1) == 2 {
				close(done)
			}
		}()
		<-ctx.Done()
		return ctx.Err()
	})
	info, err := executor.Enqueue(
		asynq.NewTask("test:timeout", nil),
		asynq.Queue("critical"), asynq.MaxRetry(1), asynq.Timeout(20*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	if info.Queue != "critical" {
		t.Fatalf("TaskInfo.Queue = %q, want critical", info.Queue)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout did not trigger both attempts")
	}
}

func TestSyncTaskExecutorFailureKeepsOriginalUniqueTTLAndArchivesStableID(t *testing.T) {
	executor := NewSyncTaskExecutor()
	base := time.Now()
	var nowMu sync.Mutex
	fakeNow := base
	executor.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return fakeNow
	}
	executor.RegisterHandler("test:unique-failure", func(context.Context, *asynq.Task) error {
		return errors.New("permanent")
	})
	newTask := func() *asynq.Task {
		return asynq.NewTask("test:unique-failure", []byte(`{"same":true}`))
	}
	opts := []asynq.Option{
		asynq.TaskID("failed-stable"), asynq.MaxRetry(0),
		asynq.Unique(10 * time.Second), asynq.Retention(time.Hour),
	}
	if _, err := executor.Enqueue(newTask(), opts...); err != nil {
		t.Fatal(err)
	}
	waitForSyncRecordToDisappear(t, executor, "failed-stable")
	executor.mu.RLock()
	_, retained := executor.retained["failed-stable"]
	_, archived := executor.archived["failed-stable"]
	executor.mu.RUnlock()
	if retained || !archived {
		t.Fatalf("failed task lifecycle retained=%v archived=%v, want false/true", retained, archived)
	}
	if _, err := executor.Enqueue(newTask(), opts...); !errors.Is(err, asynq.ErrTaskIDConflict) {
		t.Fatalf("archived stable ID error = %v, want ErrTaskIDConflict", err)
	}
	if _, err := executor.Enqueue(newTask(), asynq.MaxRetry(0), asynq.Unique(10*time.Second)); !errors.Is(err, asynq.ErrDuplicateTask) {
		t.Fatalf("failed Unique lease error = %v, want ErrDuplicateTask", err)
	}
	nowMu.Lock()
	fakeNow = base.Add(11 * time.Second)
	nowMu.Unlock()
	if _, err := executor.Enqueue(newTask(), asynq.MaxRetry(0), asynq.Unique(10*time.Second)); err != nil {
		t.Fatalf("Unique lease should expire from enqueue time: %v", err)
	}
}

func TestSyncTaskExecutorActiveOwnerKeepsUniqueLeaseBeyondTTL(t *testing.T) {
	executor := NewSyncTaskExecutor()
	base := time.Now()
	var nowMu sync.Mutex
	fakeNow := base
	executor.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return fakeNow
	}
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondStarted := make(chan struct{})
	secondRelease := make(chan struct{})
	var runs atomic.Int32
	executor.RegisterHandler("test:unique-owner", func(context.Context, *asynq.Task) error {
		switch runs.Add(1) {
		case 1:
			close(firstStarted)
			<-firstRelease
		case 2:
			close(secondStarted)
			<-secondRelease
		}
		return nil
	})
	newTask := func() *asynq.Task { return asynq.NewTask("test:unique-owner", []byte("same")) }
	opts := []asynq.Option{asynq.MaxRetry(0), asynq.Unique(time.Second)}
	if _, err := executor.Enqueue(newTask(), opts...); err != nil {
		t.Fatal(err)
	}
	<-firstStarted
	nowMu.Lock()
	fakeNow = base.Add(2 * time.Second)
	nowMu.Unlock()
	if _, err := executor.Enqueue(newTask(), opts...); !errors.Is(err, asynq.ErrDuplicateTask) {
		t.Fatalf("active owner lost Unique after TTL: error=%v", err)
	}
	close(firstRelease)
	waitForSyncTaskCount(t, executor, 0)
	if _, err := executor.Enqueue(newTask(), opts...); err != nil {
		t.Fatalf("terminal owner did not release Unique: %v", err)
	}
	<-secondStarted
	close(secondRelease)
}

func TestSyncTaskExecutorWaitsForTimedOutHandlerBeforeRetryAndUniqueRelease(t *testing.T) {
	executor := NewSyncTaskExecutor()
	executor.retryDelay = func(int) time.Duration { return 0 }
	stuckStarted := make(chan struct{})
	releaseStuck := make(chan struct{})
	secondSucceeded := make(chan struct{})
	thirdRan := make(chan struct{})
	var runs, active, maximum atomic.Int32
	executor.RegisterHandler("test:timeout-unique", func(ctx context.Context, _ *asynq.Task) error {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		switch runs.Add(1) {
		case 1:
			close(stuckStarted)
			<-releaseStuck // deliberately ignore ctx to exercise the liveness registry
			return ctx.Err()
		case 2:
			close(secondSucceeded)
			return nil
		default:
			close(thirdRan)
			return nil
		}
	})
	newTask := func() *asynq.Task { return asynq.NewTask("test:timeout-unique", []byte("same")) }
	opts := []asynq.Option{asynq.MaxRetry(1), asynq.Timeout(20 * time.Millisecond), asynq.Unique(time.Minute)}
	info, err := executor.Enqueue(newTask(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	<-stuckStarted
	select {
	case <-secondSucceeded:
		t.Fatal("retry overlapped a timed-out handler")
	case <-time.After(60 * time.Millisecond):
	}
	executor.mu.RLock()
	record := executor.tasks[info.ID]
	timedOut := record != nil && record.timedOut && record.active && record.workers == 1
	executor.mu.RUnlock()
	if !timedOut {
		t.Fatal("timed-out non-cooperative handler was not kept observable as active")
	}
	if _, err := executor.Enqueue(newTask(), asynq.MaxRetry(0), asynq.Unique(time.Minute)); !errors.Is(err, asynq.ErrDuplicateTask) {
		t.Fatalf("Unique released before timed-out handler quiesced: %v", err)
	}
	close(releaseStuck)
	select {
	case <-secondSucceeded:
	case <-time.After(time.Second):
		t.Fatal("retry did not start after timed-out handler exited")
	}
	waitForSyncTaskCount(t, executor, 0)
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent handlers = %d, want 1", maximum.Load())
	}
	if _, err := executor.Enqueue(newTask(), asynq.MaxRetry(0), asynq.Unique(time.Minute)); err != nil {
		t.Fatalf("Unique remained after terminal success: %v", err)
	}
	select {
	case <-thirdRan:
	case <-time.After(time.Second):
		t.Fatal("replacement Unique task did not run")
	}
}

func TestSyncTaskExecutorRetriesFailuresSerially(t *testing.T) {
	executor := NewSyncTaskExecutor()
	executor.retryDelay = func(int) time.Duration { return 0 }
	var attempts, active, maximum atomic.Int32
	done := make(chan struct{})
	executor.RegisterHandler("test:serial-failure", func(context.Context, *asynq.Task) error {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		run := attempts.Add(1)
		time.Sleep(10 * time.Millisecond)
		if run < 3 {
			return errors.New("retryable")
		}
		close(done)
		return nil
	})
	if _, err := executor.Enqueue(
		asynq.NewTask("test:serial-failure", nil), asynq.MaxRetry(2),
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serial retries did not complete")
	}
	waitForSyncTaskCount(t, executor, 0)
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent attempts = %d, want 1", got)
	}
}

func TestSyncTaskExecutorAbsoluteDeadlinePreventsLateRetry(t *testing.T) {
	executor := NewSyncTaskExecutor()
	executor.retryDelay = func(int) time.Duration { return 0 }
	var attempts atomic.Int32
	executor.RegisterHandler("test:absolute-deadline", func(ctx context.Context, _ *asynq.Task) error {
		attempts.Add(1)
		<-ctx.Done()
		// Simulate cancellation cleanup which completes after the absolute deadline.
		time.Sleep(25 * time.Millisecond)
		return ctx.Err()
	})
	if _, err := executor.Enqueue(
		asynq.NewTask("test:absolute-deadline", nil),
		asynq.TaskID("absolute-deadline"), asynq.MaxRetry(3),
		asynq.Deadline(time.Now().Add(20*time.Millisecond)),
	); err != nil {
		t.Fatal(err)
	}
	waitForSyncRecordToDisappear(t, executor, "absolute-deadline")
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts after absolute deadline = %d, want 1", got)
	}
	executor.mu.RLock()
	_, archived := executor.archived["absolute-deadline"]
	executor.mu.RUnlock()
	if !archived {
		t.Fatal("deadline-exhausted task was not archived")
	}
}

func TestSyncTaskExecutorRevokeWaitsForHandlerQuiescence(t *testing.T) {
	executor := NewSyncTaskExecutor()
	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	replacementRan := make(chan struct{})
	var runs atomic.Int32
	executor.RegisterHandler("test:revoke", func(ctx context.Context, _ *asynq.Task) error {
		if runs.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			close(cancelObserved)
			<-release
			return ctx.Err()
		}
		close(replacementRan)
		return nil
	})
	newTask := func() *asynq.Task { return asynq.NewTask("test:revoke", []byte("same")) }
	info, err := executor.Enqueue(newTask(), asynq.MaxRetry(4), asynq.Unique(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := executor.Revoke(info.ID); err != nil {
		t.Fatal(err)
	}
	<-cancelObserved
	if _, err := executor.Enqueue(newTask(), asynq.Unique(time.Minute)); !errors.Is(err, asynq.ErrDuplicateTask) {
		t.Fatalf("Revoke released Unique before handler exit: %v", err)
	}
	close(release)
	waitForSyncTaskCount(t, executor, 0)
	if _, err := executor.Enqueue(newTask(), asynq.MaxRetry(0), asynq.Unique(time.Minute)); err != nil {
		t.Fatalf("Revoke did not release Unique after handler exit: %v", err)
	}
	select {
	case <-replacementRan:
	case <-time.After(time.Second):
		t.Fatal("replacement task did not run after revoke quiesced")
	}
	if err := executor.Revoke("missing"); !errors.Is(err, ErrSyncTaskNotFound) {
		t.Fatalf("Revoke(missing) error = %v", err)
	}
}

func TestSyncTaskExecutorHandlerRevokeIsTerminalAndReleasesUnique(t *testing.T) {
	executor := NewSyncTaskExecutor()
	executor.retryDelay = func(int) time.Duration { return 0 }
	var attempts atomic.Int32
	executor.RegisterHandler("test:handler-revoke", func(context.Context, *asynq.Task) error {
		attempts.Add(1)
		return asynq.RevokeTask
	})
	newTask := func() *asynq.Task { return asynq.NewTask("test:handler-revoke", []byte("same")) }
	info, err := executor.Enqueue(newTask(), asynq.MaxRetry(5), asynq.Unique(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	waitForSyncRecordToDisappear(t, executor, info.ID)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts after RevokeTask = %d, want 1", got)
	}
	if _, err := executor.Enqueue(newTask(), asynq.MaxRetry(0), asynq.Unique(time.Minute)); err != nil {
		t.Fatalf("handler RevokeTask did not release Unique: %v", err)
	}
}

func TestSyncTaskExecutorShutdownKeepsNonCooperativeTaskVisible(t *testing.T) {
	executor := NewSyncTaskExecutor()
	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	executor.RegisterHandler("test:shutdown-stuck", func(ctx context.Context, _ *asynq.Task) error {
		close(started)
		<-ctx.Done()
		close(cancelObserved)
		<-release
		return ctx.Err()
	})
	newTask := func() *asynq.Task { return asynq.NewTask("test:shutdown-stuck", []byte("same")) }
	if _, err := executor.Enqueue(newTask(), asynq.Unique(time.Minute)); err != nil {
		t.Fatal(err)
	}
	<-started
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := executor.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want deadline exceeded", err)
	}
	<-cancelObserved
	executor.mu.RLock()
	remaining := len(executor.tasks)
	executor.mu.RUnlock()
	if remaining != 1 {
		t.Fatalf("non-cooperative task count = %d, want 1", remaining)
	}
	if _, err := executor.Enqueue(newTask()); !errors.Is(err, ErrSyncTaskExecutorShutdown) {
		t.Fatalf("Enqueue after Shutdown error = %v", err)
	}
	close(release)
	waitForSyncTaskCount(t, executor, 0)
	if err := executor.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown after quiescence: %v", err)
	}
}

func TestSyncTaskExecutorShutdownRacesWithEnqueueWithoutLeaks(t *testing.T) {
	executor := NewSyncTaskExecutor()
	executor.RegisterHandler("test:shutdown-race", func(ctx context.Context, _ *asynq.Task) error {
		<-ctx.Done()
		return ctx.Err()
	})

	const producers = 32
	start := make(chan struct{})
	errs := make(chan error, producers)
	var producersWG sync.WaitGroup
	for i := 0; i < producers; i++ {
		producersWG.Add(1)
		go func() {
			defer producersWG.Done()
			<-start
			_, err := executor.Enqueue(asynq.NewTask("test:shutdown-race", nil), asynq.MaxRetry(0))
			errs <- err
		}()
	}
	close(start)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := executor.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	producersWG.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrSyncTaskExecutorShutdown) {
			t.Fatalf("concurrent Enqueue error = %v", err)
		}
	}
	executor.mu.RLock()
	remaining := len(executor.tasks)
	executor.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("tasks remaining after Shutdown = %d", remaining)
	}
}

func waitForSyncRecordToDisappear(t *testing.T, executor *SyncTaskExecutor, taskID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		executor.mu.RLock()
		_, live := executor.tasks[taskID]
		executor.mu.RUnlock()
		if !live {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task %s remained live", taskID)
}

func waitForSyncTaskCount(t *testing.T, executor *SyncTaskExecutor, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		executor.mu.RLock()
		got := len(executor.tasks)
		executor.mu.RUnlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("live task count did not reach %d", want)
}

func TestSyncTaskExecutorPersistsTerminalRepairAfterExhaustion(t *testing.T) {
	executor := NewSyncTaskExecutor()
	executor.RegisterHandler(types.TypeSummaryGeneration, func(context.Context, *asynq.Task) error {
		return errors.New("finalizer database unavailable")
	})
	repairRan := make(chan types.KnowledgeTerminalRepairPayload, 1)
	executor.RegisterHandler(types.TypeKnowledgeTerminalRepair, func(_ context.Context, task *asynq.Task) error {
		var payload types.KnowledgeTerminalRepairPayload
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return err
		}
		repairRan <- payload
		return nil
	})
	originalPayload, err := json.Marshal(types.SummaryGenerationPayload{
		TenantID: 1, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1", ProcessingGeneration: "generation-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Enqueue(
		asynq.NewTask(types.TypeSummaryGeneration, originalPayload),
		asynq.MaxRetry(0),
	); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	select {
	case payload := <-repairRan:
		if payload.OriginalTaskType != types.TypeSummaryGeneration || string(payload.OriginalPayload) != string(originalPayload) {
			t.Fatalf("repair payload = %#v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal repair task did not run after Lite exhaustion")
	}
}
