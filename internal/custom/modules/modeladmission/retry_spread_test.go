package modeladmission

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSpreadProviderRetryIsStableBoundedAndNeverEarly(t *testing.T) {
	base := 5 * time.Minute
	first := SpreadProviderRetry(base, "tenant-7/kb-9/document-11")
	second := SpreadProviderRetry(base, "tenant-7/kb-9/document-11")

	require.Equal(t, first, second)
	require.GreaterOrEqual(t, first, base)
	require.Less(t, first, base+ProviderRetrySpreadWindow(base))
	require.Equal(t, base, SpreadProviderRetry(base, ""))
}

func TestSpreadProviderRetryDistributesThousandDocumentRecovery(t *testing.T) {
	const documents = 1000
	base := 5 * time.Minute
	window := ProviderRetrySpreadWindow(base)
	buckets := make(map[int64]int)
	minDelay := base + window
	maxDelay := base

	for index := 0; index < documents; index++ {
		delay := SpreadProviderRetry(base, fmt.Sprintf("tenant-7/kb-9/document-%d", index))
		require.GreaterOrEqual(t, delay, base)
		require.Less(t, delay, base+window)
		if delay < minDelay {
			minDelay = delay
		}
		if delay > maxDelay {
			maxDelay = delay
		}
		buckets[int64((delay-base)/time.Second)]++
	}

	require.GreaterOrEqual(t, len(buckets), 25,
		"one thousand documents should occupy nearly the full 30-second recovery window")
	require.Greater(t, maxDelay-minDelay, 25*time.Second)
	for second, count := range buckets {
		require.Less(t, count, 80,
			"second %d received too much of the recovery burst", second)
	}
}
