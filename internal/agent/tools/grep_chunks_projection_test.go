package tools

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"testing"

	"github.com/Tencent/WeKnora/internal/database"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Read-only integration test of the actual tool path against the source-table
// baseline, including deduplication, coverage, MMR and source-reference payloads.
func TestGrepProjectionLiveReferences(t *testing.T) {
	if os.Getenv("GREPSEARCH_TEST_LIVE") != "1" {
		t.Skip("requires migrated local database")
	}
	dsn, err := database.PostgresGormDSNFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	defer pool.Close()
	var scopeRows []struct {
		KnowledgeBaseID string
		TenantID        uint64
	}
	if err := db.Raw("SELECT DISTINCT knowledge_base_id,tenant_id FROM custom_grepsearch_chunks WHERE tenant_id=10000 ORDER BY knowledge_base_id LIMIT 18").Scan(&scopeRows).Error; err != nil {
		t.Fatal(err)
	}
	if len(scopeRows) == 0 {
		t.Fatal("local fixture tenant has no searchable data")
	}
	var targets types.SearchTargets
	var kbIDs []string
	tenants := map[string]uint64{}
	for _, r := range scopeRows {
		targets = append(targets, &types.SearchTarget{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: r.KnowledgeBaseID, TenantID: r.TenantID})
		kbIDs = append(kbIDs, r.KnowledgeBaseID)
		tenants[r.KnowledgeBaseID] = r.TenantID
	}
	scope, args := scopeClause(kbIDs, nil, nil, tenants)
	for _, query := range []string{"合同金额|立项金额|超支|变更审批|金额变更", "管理"} {
		var baseline []chunkWithTitle
		q := `SELECT chunks.id,chunks.content,chunks.chunk_index,chunks.knowledge_id,chunks.knowledge_base_id,chunks.chunk_type,chunks.metadata,chunks.source_locator,chunks.created_at,knowledges.title AS knowledge_title
            FROM chunks JOIN knowledges ON chunks.knowledge_id=knowledges.id WHERE chunks.is_enabled AND chunks.chunk_type<>'summary'
            AND chunks.deleted_at IS NULL AND knowledges.deleted_at IS NULL AND knowledges.publication_state='published'
            AND (` + scope + `) AND (chunks.content ~* ? OR knowledges.title ~* ?) ORDER BY chunks.created_at DESC,chunks.id DESC LIMIT 500`
		queryArgs := append(append([]interface{}{}, args...), query, query)
		if err := db.Raw(q, queryArgs...).Scan(&baseline).Error; err != nil {
			t.Fatal(err)
		}
		tool := NewGrepChunksTool(db, targets)
		ctx := context.Background()
		re := regexp.MustCompile("(?i)" + query)
		_, patterns := compileGrepRankingPatterns(query, re)
		scored := tool.scoreChunks(ctx, tool.deduplicateChunks(ctx, baseline), patterns)
		diverse := scored
		limit := grepResultLimit(len(patterns))
		if len(scored) > 10 {
			diverse = tool.applyMMR(ctx, scored, limit, 0.7)
		}
		expected := selectGrepCoverage(scored, diverse, patterns, limit)
		sort.Slice(expected, func(i, j int) bool { return grepResultLess(expected[i], expected[j]) })
		if len(expected) > limit {
			expected = expected[:limit]
		}
		body, _ := json.Marshal(map[string]string{"query": query})
		result, err := NewGrepChunksTool(db, targets).Execute(ctx, body)
		if err != nil || !result.Success {
			t.Fatalf("tool failed: %v", err)
		}
		if !reflect.DeepEqual(result.SourceReferences, buildGrepSourceReferences(expected, []*regexp.Regexp{re})) {
			t.Fatalf("final references differ for %q", query)
		}
		t.Logf("query=%q candidates=%d final_references=%d", query, len(baseline), len(result.SourceReferences))
	}
}
