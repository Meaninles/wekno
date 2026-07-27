package knowledgepurge

import (
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRetainWikiLogsForActiveKnowledgeSuppressesDeletionLateWrites(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`
		CREATE TABLE knowledges (
			id TEXT PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			parse_status TEXT NOT NULL,
			deleted_at DATETIME
		)
	`).Error)
	now := time.Now().UTC()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges (id, tenant_id, parse_status, deleted_at)
		 VALUES (?, ?, ?, NULL), (?, ?, ?, NULL), (?, ?, ?, ?)`,
		"active", 7, types.ParseStatusCompleted,
		"deleting", 7, types.ParseStatusDeleting,
		"deleted", 7, types.ParseStatusDeleting, now,
	).Error)

	entry := func(knowledgeID, action string) *types.WikiLogEntry {
		return &types.WikiLogEntry{
			TenantID: 7, KnowledgeBaseID: "kb-1",
			KnowledgeID: knowledgeID, Action: action,
		}
	}
	input := []*types.WikiLogEntry{
		entry("active", "retract_cancelled"),
		entry("deleting", "retract"),
		entry("deleting", "ingest"),
		entry("deleted", "retract"),
		entry("missing-legacy-source", "edit"),
		{TenantID: 7, KnowledgeBaseID: "kb-1", Action: "kb_edit"},
	}

	var retained []*types.WikiLogEntry
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var filterErr error
		retained, filterErr = RetainWikiLogsForActiveKnowledge(tx, input)
		return filterErr
	}))
	require.Equal(t, []*types.WikiLogEntry{input[0], input[4], input[5]}, retained)
}
