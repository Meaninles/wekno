package sqlite_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestWikiReverseLinkReconciliationMigration(t *testing.T) {
	baseline, err := os.ReadFile("000000_init.up.sql")
	require.NoError(t, err)
	reconcile, err := os.ReadFile("000084_reconcile_wiki_reverse_links.up.sql")
	require.NoError(t, err)

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	if _, err = db.Exec("SELECT 1"); err != nil {
		if strings.Contains(err.Error(), "CGO_ENABLED=0") {
			t.Skip("go-sqlite3 is a no-CGO stub in this shell")
		}
		require.NoError(t, err)
	}

	h := databaseHarness{db: db}
	h.exec(t, string(baseline), false)
	h.exec(t, `
		INSERT INTO wiki_pages
			(id, tenant_id, knowledge_base_id, slug, status, in_links, out_links)
		VALUES
			('target', 7, 'kb-a', 'entity/target', 'published', '["stale"]', '[]'),
			('orphan', 7, 'kb-a', 'entity/orphan', 'published', '["stale"]', '[]'),
			('source-a', 7, 'kb-a', 'entity/source-a', 'published', '[]', '["entity/target"]'),
			('source-b', 7, 'kb-a', 'entity/source-b', 'published', '[]', '["entity/target","entity/target"]'),
			('source-archived', 7, 'kb-a', 'entity/source-archived', 'archived', '[]', '["entity/target"]'),
			('other-target', 8, 'kb-b', 'entity/target', 'published', '[]', '[]'),
			('other-source', 8, 'kb-b', 'entity/other-source', 'published', '[]', '["entity/target"]');
	`, false)

	h.exec(t, string(reconcile), false)
	require.JSONEq(t, `["entity/source-a","entity/source-b"]`, h.scalar(t,
		"SELECT in_links FROM wiki_pages WHERE id = 'target'"))
	require.JSONEq(t, `[]`, h.scalar(t,
		"SELECT in_links FROM wiki_pages WHERE id = 'orphan'"))
	require.JSONEq(t, `["entity/other-source"]`, h.scalar(t,
		"SELECT in_links FROM wiki_pages WHERE id = 'other-target'"))

	// Recovery/replay must converge rather than duplicate memberships.
	h.exec(t, string(reconcile), false)
	require.JSONEq(t, `["entity/source-a","entity/source-b"]`, h.scalar(t,
		"SELECT in_links FROM wiki_pages WHERE id = 'target'"))
}
