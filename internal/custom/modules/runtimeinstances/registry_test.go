package runtimeinstances

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/runtimeprofile"
)

func TestRegistryPublishesCapabilitiesAndStopsWithBootFence(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	registry := NewRegistry(db, runtimeprofile.Profile{Role: runtimeprofile.RoleDevAll})
	require.NoError(t, registry.Migrate(context.Background()))
	require.NoError(t, registry.Start(context.Background()))

	rows, err := ListActive(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Positive(t, rows[0].DerivativeConcurrency)
	require.Positive(t, rows[0].WikiConcurrency)
	require.Positive(t, rows[0].ParseConcurrency)
	require.WithinDuration(t, time.Now().UTC(), rows[0].LastHeartbeatAt, time.Second)

	require.NoError(t, registry.Stop())
	rows, err = ListActive(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, rows)
}
