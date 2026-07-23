package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestCompareAndSwapBatchReparseSnapshotPreservesJSONTimestampPrecision(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutateTime bool
		wantSwap   bool
	}{
		{name: "exact JSON round trip", wantSwap: true},
		{name: "updated at ABA", mutateTime: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupKnowledgeTestDB(t)
			repo := NewKnowledgeRepository(db).(*knowledgeRepository)
			id, kbID := insertOwnedProcessingKnowledge(
				t, db, types.ParseStatusCompleted, "generation-old", "", 0, time.Now(),
			)
			capturedAt := time.Date(2026, 7, 22, 1, 2, 3, 456789000, time.UTC)
			require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", id).
				Update("updated_at", capturedAt).Error)
			var current types.Knowledge
			require.NoError(t, db.Where("id = ?", id).Take(&current).Error)
			snapshot := types.KnowledgeReparseExpectedSnapshot{
				TenantID: 7, KnowledgeID: id, KnowledgeBaseID: kbID,
				ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "generation-old",
				ProcessingOwner: "", UpdatedAt: current.UpdatedAt,
			}
			raw, err := json.Marshal(snapshot)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &snapshot))
			if test.mutateTime {
				require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", id).
					Update("updated_at", current.UpdatedAt.Add(time.Microsecond)).Error)
			}

			swapped, err := repo.CompareAndSwapBatchReparseSnapshot(
				context.Background(),
				7,
				id,
				kbID,
				snapshot.ParseStatus,
				snapshot.ProcessingGeneration,
				snapshot.ProcessingOwner,
				snapshot.UpdatedAt,
				map[string]interface{}{
					"parse_status":          types.ParseStatusProcessing,
					"processing_generation": "generation-batch",
					"processing_owner":      "owner-batch",
				},
			)
			require.NoError(t, err)
			require.Equal(t, test.wantSwap, swapped)
		})
	}
}
