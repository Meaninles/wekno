package derivativequeue

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFairSelectorRotatesTenantAndKnowledgeBase(t *testing.T) {
	now := time.Now().UTC()
	items := make([]WorkItem, 0, 20)
	for index := 0; index < 12; index++ {
		items = append(items, WorkItem{
			ID: fmt.Sprintf("large-%d", index), ResourcePoolID: "pool-a",
			TenantID: 1, KnowledgeBaseID: "kb-large", CreatedAt: now,
		})
	}
	items = append(items,
		WorkItem{ID: "other-kb", ResourcePoolID: "pool-a", TenantID: 1, KnowledgeBaseID: "kb-small", CreatedAt: now},
		WorkItem{ID: "other-tenant", ResourcePoolID: "pool-a", TenantID: 2, KnowledgeBaseID: "kb-two", CreatedAt: now},
		WorkItem{ID: "other-pool", ResourcePoolID: "pool-b", TenantID: 3, KnowledgeBaseID: "kb-three", CreatedAt: now},
	)
	var selector fairSelector
	selected := selector.selectCandidates(items, 6, now)
	require.Len(t, selected, 6)
	seen := map[string]bool{}
	for _, item := range selected {
		seen[item.ID] = true
	}
	require.True(t, seen["other-kb"])
	require.True(t, seen["other-tenant"])
	require.True(t, seen["other-pool"])
}

func TestFairSelectorAgingRaisesOldWorkWithinBucket(t *testing.T) {
	now := time.Now().UTC()
	items := []WorkItem{
		{ID: "new-high", ResourcePoolID: "pool", TenantID: 1, KnowledgeBaseID: "kb", Priority: 5, CreatedAt: now},
		{ID: "old-low", ResourcePoolID: "pool", TenantID: 1, KnowledgeBaseID: "kb", Priority: 0, CreatedAt: now.Add(-10 * time.Minute)},
	}
	var selector fairSelector
	selected := selector.selectCandidates(items, 1, now)
	require.Len(t, selected, 1)
	require.Equal(t, "old-low", selected[0].ID)
}
