package chatretrieval

import (
	"math"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	// GraphAnchorLimit bounds the number of entity nodes expanded by one
	// knowledge-graph query. Generic extracted terms such as "会议" can match
	// thousands of nodes in a large corpus; graph retrieval is supplementary
	// and must never become an unbounded second full-corpus scan.
	GraphAnchorLimit = 12

	// GraphRelationLimit bounds one-hop expansion before chunk materialization.
	GraphRelationLimit = 48
)

// GraphChunkBudget reserves at most one quarter of the final context for graph
// supplements. The primary vector/keyword candidates therefore retain most of
// the answer budget even when a generic entity has many graph neighbors.
func GraphChunkBudget(topK int) int {
	if topK <= 0 {
		return 8
	}
	budget := topK / 4
	if budget < 4 {
		budget = 4
	}
	if budget > 8 {
		budget = 8
	}
	return budget
}

// GraphSupplementScore keeps graph-only retrieval useful while ensuring graph
// neighbors rank strictly below every positive primary retrieval result.
func GraphSupplementScore(primary []*types.SearchResult) float64 {
	minPositive := math.MaxFloat64
	for _, result := range primary {
		if result == nil || result.Score <= 0 {
			continue
		}
		if result.Score < minPositive {
			minPositive = result.Score
		}
	}
	if minPositive == math.MaxFloat64 {
		return 1
	}
	score := minPositive * 0.5
	if score <= 0 {
		return 0.000001
	}
	return score
}

type rankedGraphNode struct {
	node         *types.GraphNode
	exact        bool
	matchedTerms int
	longestTerm  int
}

// RankGraphNodes makes graph chunk selection deterministic and favors exact or
// specific entity matches over short generic terms. Neo4j already performs the
// same ordering before relation expansion; repeating it here preserves
// deterministic ordering when results from multiple KBs complete concurrently.
func RankGraphNodes(nodes []*types.GraphNode, terms []string) []*types.GraphNode {
	ranked := make([]rankedGraphNode, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(node.Name))
		entry := rankedGraphNode{node: node}
		for _, rawTerm := range terms {
			term := strings.ToLower(strings.TrimSpace(rawTerm))
			if len([]rune(term)) < 2 || !strings.Contains(name, term) {
				continue
			}
			entry.matchedTerms++
			if name == term {
				entry.exact = true
			}
			if termLen := len([]rune(term)); termLen > entry.longestTerm {
				entry.longestTerm = termLen
			}
		}
		ranked = append(ranked, entry)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		if left.exact != right.exact {
			return left.exact
		}
		if left.matchedTerms != right.matchedTerms {
			return left.matchedTerms > right.matchedTerms
		}
		if left.longestTerm != right.longestTerm {
			return left.longestTerm > right.longestTerm
		}
		leftName := strings.ToLower(strings.TrimSpace(left.node.Name))
		rightName := strings.ToLower(strings.TrimSpace(right.node.Name))
		if len([]rune(leftName)) != len([]rune(rightName)) {
			return len([]rune(leftName)) < len([]rune(rightName))
		}
		return leftName < rightName
	})
	result := make([]*types.GraphNode, 0, len(ranked))
	for _, entry := range ranked {
		result = append(result, entry.node)
	}
	return result
}

func matchTypePriority(matchType types.MatchType) int {
	switch matchType {
	case types.MatchTypeDirectLoad:
		return 0
	case types.MatchTypeEmbedding, types.MatchTypeKeywords:
		return 1
	case types.MatchTypeParentChunk, types.MatchTypeNearByChunk:
		return 2
	case types.MatchTypeRelationChunk:
		return 3
	case types.MatchTypeGraph:
		return 4
	case types.MatchTypeHistory:
		return 5
	default:
		return 6
	}
}

// SortSearchResults restores a global relevance order after per-document merge.
// Grouping by map is deliberately unordered in Go, so filtering the first K
// entries without this step makes answer context nondeterministic.
func SortSearchResults(results []*types.SearchResult) {
	sort.SliceStable(results, func(i, j int) bool {
		left, right := results[i], results[j]
		if left == nil {
			return false
		}
		if right == nil {
			return true
		}
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		if leftPriority, rightPriority := matchTypePriority(left.MatchType),
			matchTypePriority(right.MatchType); leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if left.KnowledgeID != right.KnowledgeID {
			return left.KnowledgeID < right.KnowledgeID
		}
		if left.ChunkIndex != right.ChunkIndex {
			return left.ChunkIndex < right.ChunkIndex
		}
		return left.ID < right.ID
	})
}

// PreserveBothForPartialOverlap protects primary evidence from generated
// summaries. A summary is intentionally lossy: high lexical overlap only shows
// that both chunks discuss the same subject, not that the summary retained
// every amount, limit, exception, or sub-clause from the source. Exact content
// duplicates are already removed earlier by signature, so keeping a summary
// beside a non-summary chunk is the safe recall behavior.
func PreserveBothForPartialOverlap(left, right *types.SearchResult) bool {
	if left == nil || right == nil {
		return false
	}
	leftSummary := left.ChunkType == types.ChunkTypeSummary
	rightSummary := right.ChunkType == types.ChunkTypeSummary
	return leftSummary != rightSummary
}

// SelectRerankModelID applies an explicit tenant model first and otherwise
// selects a deterministic active/default reranker. Returning an empty string
// is safe: the pipeline retains its existing hybrid-search fallback.
func SelectRerankModelID(configured string, models []*types.Model) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	candidates := make([]*types.Model, 0)
	for _, model := range models {
		if model == nil || model.Type != types.ModelTypeRerank ||
			model.Status != types.ModelStatusActive {
			continue
		}
		candidates = append(candidates, model)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].IsDefault != candidates[j].IsDefault {
			return candidates[i].IsDefault
		}
		if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].ID
}
