package chatretrieval

import (
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestRepairReceiptDistinguishesCatalogEmptyFailureAndUnsearched(t *testing.T) {
	for _, item := range []struct {
		name     string
		count    int
		searched []string
		err      error
		status   string
	}{
		{"navigation", 2, []string{"kb"}, nil, "ok"},
		{"empty", 0, []string{"kb"}, nil, "empty"},
		{"unsearched", 0, nil, nil, "not_searched"},
		{"failure", 0, nil, errors.New("provider unavailable"), "error"},
		{"partial", 1, []string{"kb"}, errors.New("second surface failed"), "partial"},
	} {
		t.Run(item.name, func(t *testing.T) {
			result := &types.ToolResult{Success: item.err == nil, Output: "exact observed body", Data: map[string]any{"count": item.count, "search_targets": []string{"only selected document"}}}
			Receipt(result, "wiki_pages", []string{"kb"}, item.searched, []string{"query"}, item.count, item.err)
			require.Equal(t, item.status, result.Data["status"])
			require.Equal(t, false, result.Data["truncated"])
			require.Contains(t, result.Output, "exact observed body")
			require.Empty(t, result.Data["evidence_refs"], "navigation without source handles must not claim evidence")
			require.Contains(t, result.Data["scope"], "search_targets")
		})
	}
}
