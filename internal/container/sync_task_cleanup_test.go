package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/router"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
)

type captureResourceCleaner struct {
	name    string
	cleanup types.CleanupFunc
}

func (c *captureResourceCleaner) Register(cleanup types.CleanupFunc) {
	c.cleanup = cleanup
}

func (c *captureResourceCleaner) RegisterWithName(name string, cleanup types.CleanupFunc) {
	c.name = name
	c.cleanup = cleanup
}

func (*captureResourceCleaner) Cleanup(context.Context) []error { return nil }

func TestRegisterSyncTaskExecutorCleanupDrainsLiteTasks(t *testing.T) {
	executor := router.NewSyncTaskExecutor()
	started := make(chan struct{})
	executor.RegisterHandler("test:container-cleanup", func(ctx context.Context, _ *asynq.Task) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	cleaner := &captureResourceCleaner{}
	registerSyncTaskExecutorCleanup(executor, cleaner)
	if cleaner.name != "SyncTaskExecutor" || cleaner.cleanup == nil {
		t.Fatalf("cleanup registration = (%q, %v)", cleaner.name, cleaner.cleanup != nil)
	}
	if _, err := executor.Enqueue(asynq.NewTask("test:container-cleanup", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Lite task did not start")
	}
	if err := cleaner.cleanup(); err != nil {
		t.Fatalf("registered cleanup: %v", err)
	}
	if _, err := executor.Enqueue(asynq.NewTask("test:container-cleanup", nil)); !errors.Is(err, router.ErrSyncTaskExecutorShutdown) {
		t.Fatalf("Enqueue after registered cleanup error = %v", err)
	}
}

func TestRegisterSyncTaskExecutorCleanupIgnoresNilExecutor(t *testing.T) {
	cleaner := &captureResourceCleaner{}
	registerSyncTaskExecutorCleanup(nil, cleaner)
	if cleaner.cleanup != nil {
		t.Fatal("nil executor registered a cleanup")
	}
}
