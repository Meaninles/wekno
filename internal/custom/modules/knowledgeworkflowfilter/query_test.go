package knowledgeworkflowfilter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestApplyMatchesUserVisibleWorkflowStates(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`
		CREATE TABLE knowledges (
			id TEXT PRIMARY KEY,
			parse_status TEXT NOT NULL,
			summary_status TEXT,
			enrichment_status TEXT,
			wiki_status TEXT
		)
	`).Error)

	rows := []struct {
		id, parse, summary, enrichment, wiki string
	}{
		{"pending", "pending", "none", "none", "none"},
		{"core-processing", "processing", "pending", "pending", "pending"},
		{"finalizing", "finalizing", "processing", "processing", "pending"},
		{"wiki-processing", "completed", "completed", "completed", "processing"},
		{"mixed-active-failed", "completed", "failed", "completed", "processing"},
		{"summary-failed", "completed", "failed", "completed", "completed"},
		{"graph-question-degraded", "completed", "completed", "degraded", "completed"},
		{"wiki-degraded", "completed", "completed", "completed", "degraded"},
		{"hard-failure-with-degraded", "completed", "failed", "degraded", "completed"},
		{"wiki-failed", "completed", "completed", "completed", "failed"},
		{"unknown-terminal-is-failed", "completed", "completed", "unexpected", "completed"},
		{"core-failed", "failed", "failed", "none", "none"},
		{"complete", "completed", "completed", "completed", "completed"},
		{"disabled-derivatives-complete", "completed", "none", "none", "none"},
		{"null-derivatives-complete", "completed", "", "", ""},
		{"explicit-skips-complete", "completed", "skipped", "skipped", "skipped"},
		{"cancelled", "cancelled", "none", "none", "none"},
		{"cancelling", "cancelling", "none", "none", "none"},
		{"deleting", "deleting", "none", "none", "none"},
		{"draft", "draft", "none", "none", "none"},
	}
	for _, row := range rows {
		require.NoError(t, db.Exec(
			"INSERT INTO knowledges VALUES (?, ?, ?, ?, ?)",
			row.id, row.parse, row.summary, row.enrichment, row.wiki,
		).Error)
	}

	tests := []struct {
		status string
		want   []string
	}{
		{"pending", []string{"pending"}},
		{"processing", []string{"core-processing", "finalizing", "wiki-processing", "mixed-active-failed"}},
		{"degraded", []string{"graph-question-degraded", "wiki-degraded"}},
		{"failed", []string{"summary-failed", "hard-failure-with-degraded", "wiki-failed", "unknown-terminal-is-failed", "core-failed"}},
		{"completed", []string{"complete", "disabled-derivatives-complete", "null-derivatives-complete", "explicit-skips-complete"}},
		{"cancelled", []string{"cancelled"}},
		{"cancelling", []string{"cancelling"}},
		{"deleting", []string{"deleting"}},
		{"draft", []string{"draft"}},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			var ids []string
			query := Apply(db.Table("knowledges"), test.status)
			require.NoError(t, query.Order("id").Pluck("id", &ids).Error)
			assert.ElementsMatch(t, test.want, ids)
		})
	}

	// Every row must land in exactly one user-visible bucket. This catches
	// accidental overlaps between active derivative failures and terminal
	// failure/completion, as well as states present under "All statuses" but
	// impossible to select individually.
	statuses := []string{
		"pending", "processing", "completed", "degraded", "failed",
		"cancelling", "cancelled", "deleting", "draft",
	}
	seen := make(map[string]int, len(rows))
	for _, status := range statuses {
		var ids []string
		require.NoError(t, Apply(db.Table("knowledges"), status).
			Order("id").Pluck("id", &ids).Error)
		for _, id := range ids {
			seen[id]++
		}
	}
	for _, row := range rows {
		assert.Equalf(t, 1, seen[row.id], "row %s must match exactly one workflow status", row.id)
	}
}
