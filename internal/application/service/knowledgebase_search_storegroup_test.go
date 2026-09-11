package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/retrievalfence"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPartitionKnowledgeBasesForSearch_CoalescesUnboundAcrossTenants(t *testing.T) {
	t.Parallel()

	boundStore := "store-a"
	kbs := []*types.KnowledgeBase{
		{ID: "env-tenant-1", TenantID: 1},
		{ID: "env-tenant-2", TenantID: 2},
		{ID: "bound-tenant-1", TenantID: 1, VectorStoreID: &boundStore},
	}

	buckets := partitionKnowledgeBasesForSearch(kbs)
	require.Len(t, buckets, 2,
		"unbound KBs share the request-scoped environment engine")

	envKBs := buckets[storeGroupPartitionKey{}]
	require.Len(t, envKBs, 2)
	assert.Equal(t, []string{"env-tenant-1", "env-tenant-2"},
		[]string{envKBs[0].ID, envKBs[1].ID})

	boundKBs := buckets[storeGroupPartitionKey{
		storeID:       boundStore,
		ownerTenantID: 1,
	}]
	require.Len(t, boundKBs, 1)
	assert.Equal(t, "bound-tenant-1", boundKBs[0].ID)
}

func TestPartitionKnowledgeBasesForSearch_KeepsBoundStoreOwnershipPartitions(t *testing.T) {
	t.Parallel()

	storeA := "store-a"
	storeB := "store-b"
	kbs := []*types.KnowledgeBase{
		{ID: "a-tenant-1", TenantID: 1, VectorStoreID: &storeA},
		{ID: "a-tenant-2", TenantID: 2, VectorStoreID: &storeA},
		{ID: "b-tenant-1", TenantID: 1, VectorStoreID: &storeB},
		{ID: "env", TenantID: 99},
	}

	buckets := partitionKnowledgeBasesForSearch(kbs)
	require.Len(t, buckets, 4,
		"different bound stores or owners must not be merged")
	assert.Len(t, buckets[storeGroupPartitionKey{
		storeID:       storeA,
		ownerTenantID: 1,
	}], 1)
	assert.Len(t, buckets[storeGroupPartitionKey{
		storeID:       storeA,
		ownerTenantID: 2,
	}], 1)
	assert.Len(t, buckets[storeGroupPartitionKey{
		storeID:       storeB,
		ownerTenantID: 1,
	}], 1)
	assert.Len(t, buckets[storeGroupPartitionKey{}], 1)
}

func TestGroupScopes_PreservesPerKnowledgeBaseTenantBoundary(t *testing.T) {
	t.Parallel()

	group := &storeGroup{
		OwnerTenantID: 1,
		KBIDs:         []string{"kb-1", "kb-2"},
		Scopes: []retrievalfence.Scope{
			{TenantID: 1, KnowledgeBaseID: "kb-1"},
			{TenantID: 2, KnowledgeBaseID: "kb-2"},
		},
	}

	got := groupScopes(group)
	require.Len(t, got, 2)
	assert.Equal(t, uint64(1), got[0].TenantID)
	assert.Equal(t, uint64(2), got[1].TenantID)

	// The retrieval path must not expose the group's internal scope slice to
	// downstream code that might mutate it.
	got[0].TenantID = 99
	assert.Equal(t, uint64(1), group.Scopes[0].TenantID)
}
