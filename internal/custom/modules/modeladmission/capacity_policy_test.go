package modeladmission

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func canonicalTestPool() ResourcePool {
	return ResourcePool{
		ID: "pool", Name: "route", ResourceKind: "chat",
		MaxInflight: 4, InteractiveReserve: 1,
		TenantBurst: 4, DocumentBurst: 2,
		RequestTimeoutSeconds: 900,
		CircuitThreshold:      3, CircuitWindowSeconds: 600, CircuitOpenSeconds: 60,
		State: "enabled",
	}
}

func TestNormalizePoolUsesOneConcurrencySource(t *testing.T) {
	pool := canonicalTestPool()
	pool.MaxBackgroundInflight = 99
	pool.TenantGuaranteed = 3
	pool.DocumentGuaranteed = 2
	pool.TokenBurst = 1234
	chatLimit := 9
	pool.ChatMaxConcurrent = &chatLimit
	require.NoError(t, NormalizePool(&pool))
	require.Equal(t, 3, pool.MaxBackgroundInflight)
	require.Equal(t, 1, pool.TenantGuaranteed)
	require.Equal(t, 1, pool.DocumentGuaranteed)
	require.Zero(t, pool.TokenBurst)
	require.Nil(t, pool.ChatMaxConcurrent)
}

func TestNormalizePoolRejectsCrossLayerConflicts(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ResourcePool)
	}{
		{"no background capacity", func(pool *ResourcePool) { pool.InteractiveReserve = pool.MaxInflight }},
		{"tenant exceeds provider", func(pool *ResourcePool) { pool.TenantBurst = pool.MaxInflight + 1 }},
		{"document exceeds tenant", func(pool *ResourcePool) { pool.TenantBurst, pool.DocumentBurst = 1, 2 }},
		{"document exceeds background", func(pool *ResourcePool) { pool.InteractiveReserve, pool.DocumentBurst = 3, 2 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := canonicalTestPool()
			test.edit(&pool)
			require.Error(t, NormalizePool(&pool))
		})
	}
}
