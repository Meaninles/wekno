package derivativequeue

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
)

type summaryReconcileKnowledge struct {
	ID                   string `gorm:"primaryKey"`
	TenantID             uint64
	KnowledgeBaseID      string
	ProcessingGeneration string
	SummaryStatus        string
	UpdatedAt            time.Time
	DeletedAt            gorm.DeletedAt
}

func (summaryReconcileKnowledge) TableName() string { return "knowledges" }

func TestReconcileStaleSummariesUsesDurableStateOnly(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&summaryReconcileKnowledge{}, &WorkItem{}))
	old := time.Now().Add(-2 * time.Hour)
	for _, id := range []string{"active", "done", "failed", "missing"} {
		require.NoError(t, db.Create(&summaryReconcileKnowledge{
			ID: id, TenantID: 7, KnowledgeBaseID: "kb-1",
			ProcessingGeneration: "generation-1",
			SummaryStatus:        types.SummaryStatusProcessing, UpdatedAt: old,
		}).Error)
	}
	for id, state := range map[string]string{
		"active": StateRetryWait, "done": StateCompleted, "failed": StateProviderUnknown,
	} {
		require.NoError(t, db.Create(&WorkItem{
			ID: id + "-work", TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: id,
			ProcessingGeneration: "generation-1", ItemID: "summary", WorkKind: WorkSummary,
			Payload: types.JSON(`{}`), PayloadHash: "hash-" + id,
			State: state, NextAttemptAt: old, CreatedAt: old, UpdatedAt: old,
		}).Error)
	}
	var candidates []types.Knowledge
	require.NoError(t, db.Table("knowledges").Find(&candidates).Error)
	stats, err := ReconcileStaleSummaries(context.Background(), db, candidates, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, stats.Preserved)
	require.EqualValues(t, 1, stats.Completed)
	require.EqualValues(t, 2, stats.Failed)

	want := map[string]string{
		"active":  types.SummaryStatusProcessing,
		"done":    types.SummaryStatusCompleted,
		"failed":  types.SummaryStatusFailed,
		"missing": types.SummaryStatusFailed,
	}
	for id, status := range want {
		var row summaryReconcileKnowledge
		require.NoError(t, db.First(&row, "id = ?", id).Error)
		require.Equal(t, status, row.SummaryStatus, id)
	}
}
