package maintenance

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLeaderHooksStartInOrderAndStopInReverse(t *testing.T) {
	coordinator := &Coordinator{}
	var mu sync.Mutex
	events := make([]string, 0, 4)
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	require.NoError(t, coordinator.Register(Hook{
		Name:  "first",
		Start: func(context.Context) error { record("start:first"); return nil },
		Stop:  func() { record("stop:first") },
	}))
	require.NoError(t, coordinator.Register(Hook{
		Name:  "second",
		Start: func(context.Context) error { record("start:second"); return nil },
		Stop:  func() { record("stop:second") },
	}))

	started := coordinator.startHooks(context.Background())
	coordinator.stopHooks(started)

	require.Equal(t, []string{
		"start:first", "start:second", "stop:second", "stop:first",
	}, events)
}

func TestLeaderHooksRejectDuplicatesAndSkipFailedStart(t *testing.T) {
	coordinator := &Coordinator{}
	require.NoError(t, coordinator.Register(Hook{
		Name: "scheduler",
		Start: func(context.Context) error {
			return errors.New("unavailable")
		},
	}))
	require.Error(t, coordinator.Register(Hook{
		Name:  "scheduler",
		Start: func(context.Context) error { return nil },
	}))
	require.Empty(t, coordinator.startHooks(context.Background()))
}
