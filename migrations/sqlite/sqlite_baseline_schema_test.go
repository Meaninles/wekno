package sqlite_test

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// TestBaselineReliabilitySchemaDatabaseDriver is the CI guard: it exercises
// the same database/sql driver used by the production SQLite migrator and
// executes the real multi-statement migration file without a shell parser.
func TestBaselineReliabilitySchemaDatabaseDriver(t *testing.T) {
	up, down := readBaselineMigrations(t)
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	if _, err = db.Exec("SELECT 1"); err != nil {
		// The Windows development shell intentionally sets CGO_ENABLED=0. The
		// Linux app/CI build has CGO and must execute this test; the companion
		// CLI test below still validates the exact SQL on no-CGO workstations.
		if strings.Contains(err.Error(), "CGO_ENABLED=0") {
			t.Skip("go-sqlite3 is a no-CGO stub in this shell")
		}
		require.NoError(t, err)
	}
	h := databaseHarness{db: db}
	h.exec(t, "PRAGMA foreign_keys = ON", false)
	verifyBaselineReliabilitySchema(t, h, up, down)
}

// TestBaselineReliabilitySchemaCLI is an independent parser check for local
// no-CGO workstations. It is supplementary; CI coverage comes from the
// production database driver test above.
func TestBaselineReliabilitySchemaCLI(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI is unavailable")
	}
	up, down := readBaselineMigrations(t)
	h := cliHarness{dbPath: filepath.Join(t.TempDir(), "baseline.db")}
	verifyBaselineReliabilitySchema(t, h, up, down)
}

func readBaselineMigrations(t *testing.T) ([]byte, []byte) {
	t.Helper()
	up, err := os.ReadFile("000000_init.up.sql")
	require.NoError(t, err)
	down, err := os.ReadFile("000000_init.down.sql")
	require.NoError(t, err)
	return up, down
}

// verifyBaselineReliabilitySchema protects the consolidated Lite schema from
// drifting behind PostgreSQL's durable document and Wiki pipeline migrations.
func verifyBaselineReliabilitySchema(t *testing.T, h sqliteHarness, up, down []byte) {
	h.exec(t, string(up), false)
	// Version-zero dirty-state recovery replays this file, so every schema
	// statement must remain idempotent.
	h.exec(t, string(up), false)

	for _, table := range []string{
		"custom_wiki_ingest_leases",
		"task_pending_ops",
		"task_dead_letters",
		"wiki_pages",
		"wiki_folders",
		"wiki_page_issues",
		"wiki_log_entries",
		"knowledges",
		"knowledge_fanout_completions",
		"knowledge_processing_spans",
		"system_settings",
	} {
		require.Equal(t, "1", h.scalar(t,
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = "+quoteSQL(table)))
	}

	requireColumns(t, h, "knowledge_bases", "wiki_config", "indexing_strategy")
	requireColumns(t, h, "knowledges",
		"pending_subtasks_count", "processing_generation", "processing_owner",
		"processing_workflow_id", "processing_fanout")
	requireColumns(t, h, "wiki_log_entries", "source_op_id")
	requireColumns(t, h, "users", "is_system_admin")
	requireColumns(t, h, "task_pending_ops", "map_ready_at")

	for _, index := range []string{
		"idx_task_pending_ops_scope_op_dedup",
		"idx_task_pending_ops_wiki_commit_ready",
		"idx_task_pending_ops_wiki_map_pending",
		"idx_task_pending_ops_wiki_retry_rotation",
		"uq_task_pending_ops_wiki_ingest",
		"uq_task_pending_ops_wiki_retract",
		"uq_wiki_log_entries_source_op_id",
		"idx_knowledges_processing_generation",
		"idx_knowledges_processing_workflow",
		"idx_knowledge_fanout_completion_generation",
		"uq_task_pending_ops_knowledge_aux_owned",
	} {
		require.Equal(t, "1", h.scalar(t,
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = "+quoteSQL(index)))
	}

	// Representative writes verify constraints, not merely the presence of
	// similarly named tables and indexes.
	h.exec(t, `
		INSERT INTO knowledges
			(id, tenant_id, knowledge_base_id, type, title, source, parse_status, processing_generation)
		VALUES ('knowledge-1', 7, 'kb-1', 'file', 'doc', 'local://doc', 'processing', 'generation-1');
	`, false)
	require.Equal(t, "0", h.scalar(t,
		"SELECT pending_subtasks_count FROM knowledges WHERE id = 'knowledge-1'"))

	h.exec(t, `
		INSERT INTO knowledge_fanout_completions
			(tenant_id, knowledge_id, knowledge_base_id, processing_generation, item_id)
		VALUES (7, 'knowledge-1', 'kb-1', 'generation-1', 'summary');
		INSERT INTO knowledge_processing_spans
			(knowledge_id, attempt, span_id, name, kind, status)
		VALUES ('knowledge-1', 1, 'span-1', 'postprocess', 'stage', 'done');
	`, false)
	h.exec(t, `
		INSERT INTO knowledge_processing_spans
			(knowledge_id, attempt, span_id, name, kind, status)
		VALUES ('knowledge-1', 1, 'span-1', 'postprocess', 'stage', 'done');
	`, true)

	retract := `INSERT INTO task_pending_ops
		(tenant_id, task_type, scope, scope_id, op, dedup_key, payload)
		VALUES (7, 'wiki:ingest', 'knowledge_base', 'kb-1', 'retract', 'knowledge-1', '{}');`
	h.exec(t, retract, false)
	h.exec(t, retract, true)
	// One exact generation is idempotent; a new generation uses a distinct
	// dedup key and remains independently queueable.
	ingest := `INSERT INTO task_pending_ops
		(tenant_id, task_type, scope, scope_id, op, dedup_key, payload)
		VALUES (7, 'wiki:ingest', 'knowledge_base', 'kb-1', 'ingest', 'knowledge-1:generation-1', '{}');`
	h.exec(t, ingest, false)
	h.exec(t, ingest, true)
	h.exec(t, `INSERT INTO task_pending_ops
		(tenant_id, task_type, scope, scope_id, op, dedup_key, payload)
		VALUES (7, 'wiki:ingest', 'knowledge_base', 'kb-1', 'ingest', 'knowledge-1:generation-2', '{}');`, false)
	owned := `INSERT INTO task_pending_ops
		(tenant_id, task_type, scope, scope_id, op, dedup_key, payload)
		VALUES (7, 'knowledge:aux_object', 'knowledge_base', 'kb-1', 'owned', 'knowledge-1/path', '{}');`
	h.exec(t, owned, false)
	h.exec(t, owned, true)

	logWithSource := `INSERT INTO wiki_log_entries
		(source_op_id, tenant_id, knowledge_base_id, action)
		VALUES (99, 7, 'kb-1', 'ingest');`
	h.exec(t, logWithSource, false)
	h.exec(t, logWithSource, true)
	h.exec(t, `
		INSERT INTO wiki_log_entries (tenant_id, knowledge_base_id, action) VALUES (7, 'kb-1', 'admin');
		INSERT INTO wiki_log_entries (tenant_id, knowledge_base_id, action) VALUES (7, 'kb-1', 'admin');
	`, false)

	h.exec(t, `
		INSERT INTO wiki_pages (id, tenant_id, knowledge_base_id, slug)
		VALUES ('page-1', 7, 'kb-1', 'overview');
	`, false)
	h.exec(t, `
		INSERT INTO wiki_pages (id, tenant_id, knowledge_base_id, slug)
		VALUES ('page-duplicate', 7, 'kb-1', 'overview');
	`, true)
	h.exec(t, `
		UPDATE wiki_pages SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'page-1';
		INSERT INTO wiki_pages (id, tenant_id, knowledge_base_id, slug)
		VALUES ('page-replacement', 7, 'kb-1', 'overview');
	`, false)

	h.exec(t, `
		INSERT INTO custom_wiki_ingest_leases
			(tenant_id, knowledge_base_id, epoch, lease_token)
		VALUES (7, 'kb-1', 0, 'short');
	`, true)
	h.exec(t, `
		INSERT INTO custom_wiki_ingest_leases
			(tenant_id, knowledge_base_id, epoch, lease_token)
		VALUES (7, 'kb-1', 1, '0123456789abcdef0123456789abcdef');
		INSERT INTO system_settings (key, value, value_type, category)
		VALUES ('max_file_size_mb', '50', 'int', 'limits')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;
	`, false)

	// Down must cover every consolidated table and remain valid with foreign
	// key enforcement enabled.
	h.exec(t, string(down), false)
	require.Equal(t, "0", h.scalar(t, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
	`))
}

type sqliteHarness interface {
	exec(t *testing.T, script string, wantError bool)
	scalar(t *testing.T, query string) string
}

type databaseHarness struct {
	db *sql.DB
}

func (h databaseHarness) exec(t *testing.T, script string, wantError bool) {
	t.Helper()
	_, err := h.db.Exec(script)
	if wantError {
		require.Error(t, err, "sqlite script unexpectedly succeeded")
		return
	}
	require.NoError(t, err)
}

func (h databaseHarness) scalar(t *testing.T, query string) string {
	t.Helper()
	var value string
	require.NoError(t, h.db.QueryRow(query).Scan(&value))
	return strings.TrimSpace(value)
}

type cliHarness struct {
	dbPath string
}

func (h cliHarness) exec(t *testing.T, script string, wantError bool) {
	t.Helper()
	cmd := exec.Command("sqlite3", "-batch", h.dbPath)
	cmd.Stdin = strings.NewReader(".bail on\nPRAGMA foreign_keys = ON;\n" + script)
	output, err := cmd.CombinedOutput()
	if wantError {
		require.Error(t, err, "sqlite script unexpectedly succeeded")
		return
	}
	require.NoError(t, err, "sqlite script failed: %s", strings.TrimSpace(string(output)))
}

func (h cliHarness) scalar(t *testing.T, query string) string {
	t.Helper()
	cmd := exec.Command("sqlite3", "-batch", "-noheader", h.dbPath, query)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "sqlite query failed: %s", strings.TrimSpace(string(output)))
	return strings.TrimSpace(string(output))
}

func requireColumns(t *testing.T, h sqliteHarness, table string, columns ...string) {
	t.Helper()
	quoted := make([]string, 0, len(columns))
	for _, column := range columns {
		quoted = append(quoted, quoteSQL(column))
	}
	query := "SELECT COUNT(*) FROM pragma_table_info(" + quoteSQL(table) + ") WHERE name IN (" +
		strings.Join(quoted, ",") + ")"
	require.Equal(t, strconv.Itoa(len(columns)), h.scalar(t, query))
}

func quoteSQL(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
