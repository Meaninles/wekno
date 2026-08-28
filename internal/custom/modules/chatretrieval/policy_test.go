package chatretrieval

import (
	"strconv"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestGraphChunkBudgetIsBounded(t *testing.T) {
	tests := []struct {
		topK int
		want int
	}{
		{topK: 0, want: 8},
		{topK: 4, want: 4},
		{topK: 30, want: 7},
		{topK: 100, want: 8},
	}
	for _, test := range tests {
		if got := GraphChunkBudget(test.topK); got != test.want {
			t.Fatalf("GraphChunkBudget(%d) = %d, want %d", test.topK, got, test.want)
		}
	}
}

func TestGraphSupplementScoreNeverOutranksPrimary(t *testing.T) {
	primary := []*types.SearchResult{
		{Score: 0.016},
		{Score: 0.009},
	}
	got := GraphSupplementScore(primary)
	if got <= 0 || got >= 0.009 {
		t.Fatalf("GraphSupplementScore() = %f, want 0 < score < 0.009", got)
	}
	if got := GraphSupplementScore(nil); got != 1 {
		t.Fatalf("GraphSupplementScore(nil) = %f, want 1", got)
	}
}

func TestRankGraphNodesPrefersExactAndSpecificMatches(t *testing.T) {
	nodes := []*types.GraphNode{
		{Name: "评审会议"},
		{Name: "会议"},
		{Name: "年度生产会议"},
	}
	got := RankGraphNodes(nodes, []string{"会议", "评审会议"})
	if got[0].Name != "评审会议" {
		t.Fatalf("first node = %q, want exact specific match", got[0].Name)
	}
	if got[1].Name != "会议" {
		t.Fatalf("second node = %q, want exact generic match", got[1].Name)
	}
}

func TestSortSearchResultsIsGloballyDeterministic(t *testing.T) {
	results := []*types.SearchResult{
		{ID: "graph", KnowledgeID: "b", Score: 0.004, MatchType: types.MatchTypeGraph},
		{ID: "vector-b", KnowledgeID: "b", ChunkIndex: 2, Score: 0.016, MatchType: types.MatchTypeEmbedding},
		{ID: "vector-a", KnowledgeID: "a", ChunkIndex: 1, Score: 0.016, MatchType: types.MatchTypeEmbedding},
	}
	SortSearchResults(results)
	got := []string{results[0].ID, results[1].ID, results[2].ID}
	want := []string{"vector-a", "vector-b", "graph"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted IDs = %v, want %v", got, want)
		}
	}
}

func TestPromoteExactStructuralMatchesRestoresFilteredArticle(t *testing.T) {
	article15 := &types.SearchResult{ID: "article-15", Content: "第十五条 采购计划", Score: 0.91}
	article33 := &types.SearchResult{ID: "article-33", Content: "第三十三条 公开采购通过采购公告邀请不特定供应商", Score: 0.12}
	got := PromoteExactStructuralMatches(
		"只根据第三十三条回答",
		[]*types.SearchResult{article15, article33},
		[]*types.SearchResult{article15},
	)
	if len(got) != 2 || got[0].ID != "article-33" {
		t.Fatalf("promoted results = %#v, want article-33 first", got)
	}
	if got[0].Score != 1.0 || got[0].Metadata["exact_structural_identifier"] != "true" {
		t.Fatalf("exact candidate not marked/promoted: %#v", got[0])
	}
}

func TestPromoteExactStructuralMatchesNormalizesSpacesAndFullWidthDigits(t *testing.T) {
	article := &types.SearchResult{ID: "article-33", Content: "第33条 正文", Score: 0.2}
	got := PromoteExactStructuralMatches(
		"查找第 ３３ 条",
		[]*types.SearchResult{article},
		nil,
	)
	if len(got) != 1 || got[0].ID != "article-33" {
		t.Fatalf("full-width exact identifier was not matched: %#v", got)
	}
}

func TestPromoteExactStructuralMatchesDoesNotChangeSemanticQuery(t *testing.T) {
	ranked := []*types.SearchResult{{ID: "existing", Content: "第三十三条", Score: 0.5}}
	got := PromoteExactStructuralMatches("公开采购如何邀请供应商", ranked, ranked)
	if len(got) != 1 || got[0].Score != 0.5 || got[0].Metadata != nil {
		t.Fatalf("non-structural query changed: %#v", got)
	}
}

func TestPromoteExplicitCoverageMatchesRestoresEachNamedAspect(t *testing.T) {
	inquiry := &types.SearchResult{ID: "inquiry", Content: "第三十五条 询比采购适用条件", Score: 0.20}
	auction := &types.SearchResult{ID: "auction", Content: "第三十六条 竞价采购适用条件", Score: 0.19}
	negotiation := &types.SearchResult{ID: "negotiation", Content: "第三十七条 竞争谈判适用条件", Score: 0.18}
	unrelated := &types.SearchResult{ID: "unrelated", Content: "采购项目通用流程", Score: 0.95}

	got := PromoteExplicitCoverageMatches(
		"比较询比、竞价、竞争谈判的适配点与风险",
		[]*types.SearchResult{unrelated, inquiry, auction, negotiation},
		[]*types.SearchResult{unrelated},
	)
	seen := map[string]bool{}
	for _, item := range got {
		seen[item.ID] = true
	}
	for _, id := range []string{"inquiry", "auction", "negotiation", "unrelated"} {
		if !seen[id] {
			t.Fatalf("named aspect %q missing from promoted results: %#v", id, got)
		}
	}
}

func TestPromoteExplicitCoverageMatchesIgnoresInstructionWordsAndAllowsModerateDF(t *testing.T) {
	inquiry := &types.SearchResult{ID: "inquiry", Content: "第三十五条 询比采购，是指一次报价；符合下列适用条件", Score: 0.10}
	auction := &types.SearchResult{ID: "auction", Content: "第三十六条 竞价采购，是指多次报价；符合下列适用条件", Score: 0.10}
	negotiation := &types.SearchResult{ID: "negotiation", Content: "第三十七条 竞争谈判，是指协商报价；符合下列适用条件", Score: 0.10}
	list := &types.SearchResult{ID: "list", Content: "采购方式包括询比采购、竞价采购和竞争谈判", Score: 0.10}
	generic := &types.SearchResult{ID: "generic", Content: "对文件内容进行具体比较后回答", Score: 0.99}
	candidates := []*types.SearchResult{generic, list, inquiry, auction, negotiation}

	got := PromoteExplicitCoverageMatches(
		"请具体比较询比采购、竞价采购和竞争谈判的定义与适用重点，每种一段。",
		candidates,
		nil,
	)
	seen := map[string]bool{}
	for _, item := range got {
		seen[item.ID] = true
		if topic := item.Metadata["explicit_query_topic"]; topic == "具体" || topic == "比较" {
			t.Fatalf("instruction word was promoted as a topic: %#v", item)
		}
	}
	for _, id := range []string{"inquiry", "auction", "negotiation"} {
		if !seen[id] {
			t.Fatalf("moderately frequent named topic %q was not restored: %#v", id, got)
		}
	}
}

func TestPromoteExplicitCoverageMatchesKeepsExplicitCompoundWithHighSegmentDF(t *testing.T) {
	inquiry := &types.SearchResult{ID: "inquiry", Content: "第三十五条 询比采购，是指一次性报出不可更改价格", Score: 0.10}
	auction := &types.SearchResult{ID: "auction", Content: "第三十六条 竞价采购，是指多次竞争报价", Score: 0.10}
	definition := &types.SearchResult{ID: "negotiation-definition", Content: "第三十七条 竞争谈判是指与二家以上符合资格条件的供应商洽谈", Score: 0.08}
	conditions := &types.SearchResult{ID: "negotiation-conditions", Content: "竞争谈判适用条件包括技术复杂和不同路径", Score: 0.09}
	candidates := []*types.SearchResult{inquiry, auction, definition, conditions}
	// Make the segmented token “谈判” too frequent for the ordinary DF guard.
	for i := 0; i < 7; i++ {
		candidates = append(candidates, &types.SearchResult{
			ID:      "noise-" + strconv.Itoa(i),
			Content: "通用谈判流程和会议记录",
			Score:   0.5,
		})
	}

	got := PromoteExplicitCoverageMatches(
		"请依据制度比较询比、竞价和竞争谈判的定义与适用重点，每种一段。",
		candidates,
		nil,
	)
	seen := map[string]bool{}
	for _, item := range got {
		seen[item.ID] = true
	}
	for _, id := range []string{"inquiry", "auction", "negotiation-definition"} {
		if !seen[id] {
			t.Fatalf("explicit compound aspect %q was not restored: %#v", id, got)
		}
	}
}

func TestPromoteExplicitCoverageMatchesLeavesSingleTopicQueryAlone(t *testing.T) {
	ranked := []*types.SearchResult{{ID: "existing", Content: "竞价采购", Score: 0.5}}
	got := PromoteExplicitCoverageMatches("竞价采购是什么", ranked, ranked)
	if len(got) != 1 || got[0].Score != 0.5 || got[0].Metadata != nil {
		t.Fatalf("single-topic query changed: %#v", got)
	}
}

func TestPreserveBothForPartialOverlapKeepsRawEvidenceBesideSummary(t *testing.T) {
	summary := &types.SearchResult{ChunkType: types.ChunkTypeSummary}
	raw := &types.SearchResult{ChunkType: types.ChunkTypeText}
	if !PreserveBothForPartialOverlap(summary, raw) {
		t.Fatal("generated summary must not evict raw source evidence")
	}
	if !PreserveBothForPartialOverlap(raw, summary) {
		t.Fatal("policy must be symmetric")
	}
	if PreserveBothForPartialOverlap(
		raw,
		&types.SearchResult{ChunkType: types.ChunkTypeText},
	) {
		t.Fatal("two raw chunks should still use normal overlap deduplication")
	}
}

func TestSelectRerankModelIDHonorsConfigAndActiveDefault(t *testing.T) {
	now := time.Now()
	models := []*types.Model{
		{
			ID:        "inactive",
			Type:      types.ModelTypeRerank,
			Status:    types.ModelStatusDownloadFailed,
			CreatedAt: now.Add(-time.Hour),
		},
		{
			ID:        "old-active",
			Type:      types.ModelTypeRerank,
			Status:    types.ModelStatusActive,
			CreatedAt: now.Add(-2 * time.Hour),
		},
		{
			ID:        "default-active",
			Type:      types.ModelTypeRerank,
			Status:    types.ModelStatusActive,
			IsDefault: true,
			CreatedAt: now,
		},
	}
	if got := SelectRerankModelID("configured", models); got != "configured" {
		t.Fatalf("configured selection = %q", got)
	}
	if got := SelectRerankModelID("", models); got != "default-active" {
		t.Fatalf("fallback selection = %q, want default-active", got)
	}
}
