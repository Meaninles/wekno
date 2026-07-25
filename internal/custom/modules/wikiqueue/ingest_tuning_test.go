package wikiqueue

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestResolveIngestBatchSizeBoundsSettlementToOneMapWave(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   *types.WikiConfig
		fallback int
		want     int
	}{
		{
			name:     "nil config uses conservative map default",
			fallback: 5,
			want:     DefaultMapParallel,
		},
		{
			name: "large configured batch cannot create map head of line waves",
			config: &types.WikiConfig{
				IngestBatchSize:   20,
				IngestMapParallel: 4,
			},
			fallback: 5,
			want:     4,
		},
		{
			name: "smaller configured batch remains authoritative",
			config: &types.WikiConfig{
				IngestBatchSize:   3,
				IngestMapParallel: 8,
			},
			fallback: 5,
			want:     3,
		},
		{
			name: "raising batch and map together remains supported",
			config: &types.WikiConfig{
				IngestBatchSize:   12,
				IngestMapParallel: 12,
			},
			fallback: 5,
			want:     12,
		},
		{
			name:     "invalid fallback is still positive",
			config:   &types.WikiConfig{},
			fallback: 0,
			want:     1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, ResolveIngestBatchSize(tt.config, tt.fallback))
		})
	}
}
