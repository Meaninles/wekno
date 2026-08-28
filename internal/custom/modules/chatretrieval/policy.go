package chatretrieval

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

var structuralIdentifierPattern = regexp.MustCompile(
	`第\s*[0-9０-９零〇一二三四五六七八九十百千万两]+\s*[编章节条款项]`,
)

var multiAspectQueryMarkers = []string{
	"比较", "对比", "分别", "每种", "每个", "逐项", "逐一", "列出", "以下", "哪些",
	"compare", "each", "respectively", "list",
}

var explicitListSeparatorPattern = regexp.MustCompile(
	`(?:、|，|,|；|;|/|\s+(?:and|versus|vs\.?)\s+|和|与|及)`,
)

var lexicalCoverageStopwords = map[string]struct{}{
	"采购": {}, "项目": {}, "制度": {}, "方式": {}, "条件": {}, "影响": {}, "确认": {},
	"服务": {}, "公开": {}, "需求": {}, "时间": {}, "风险": {}, "事实": {}, "当前": {},
	"系统": {}, "可以": {}, "至少": {}, "进行": {}, "根据": {}, "依据": {}, "选择": {},
	"回答": {}, "支持": {}, "来源": {}, "信息": {}, "引用": {}, "最终": {}, "适配": {},
	"情况": {}, "内容": {}, "相关": {}, "问题": {}, "说明": {}, "分析": {}, "判断": {},
	"比较": {}, "对比": {}, "定义": {}, "适用": {}, "重点": {}, "具体": {}, "结合": {},
	"暂不": {}, "不要": {}, "停止": {}, "讨论": {}, "临时": {}, "只回答": {}, "就近": {},
	"每种": {}, "每个": {}, "一段": {}, "一行": {}, "列出": {}, "服务项目": {},
	"竞争": {}, // Too broad alone: prefer a longer query token such as “竞争谈判”.
}

type coverageTokenCandidate struct {
	token    string
	df       int
	explicit bool
}

const noCoverageCandidateScore = -1 << 30

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

// PromoteExactStructuralMatches protects exact article/section lookups from a
// semantic reranker false negative. Retrieval has already enforced tenant and
// knowledge scope; this policy only restores up to a small bounded number of
// candidates whose source text contains an identifier explicitly named by the
// user (for example “第三十三条”). It does not fabricate a candidate or bypass
// source authorization.
func PromoteExactStructuralMatches(
	query string,
	candidates []*types.SearchResult,
	ranked []*types.SearchResult,
) []*types.SearchResult {
	identifiers := structuralIdentifiers(query)
	if len(identifiers) == 0 || len(candidates) == 0 {
		return ranked
	}

	limit := len(identifiers) * 2
	if limit < 2 {
		limit = 2
	}
	if limit > 8 {
		limit = 8
	}

	seen := make(map[string]struct{}, len(ranked))
	promoted := make([]*types.SearchResult, 0, limit)
	for _, candidate := range candidates {
		if candidate == nil || !containsStructuralIdentifier(candidate.Content, identifiers) {
			continue
		}
		key := searchResultIdentity(candidate)
		if _, exists := seen[key]; exists {
			continue
		}
		candidate.Metadata = ensureSearchMetadata(candidate.Metadata)
		candidate.Metadata["exact_structural_identifier"] = "true"
		candidate.Metadata["pre_exact_promotion_score"] = strconv.FormatFloat(candidate.Score, 'f', 6, 64)
		candidate.Score = 1.0
		promoted = append(promoted, candidate)
		seen[key] = struct{}{}
		if len(promoted) == limit {
			break
		}
	}

	// Exact candidates lead deterministically. Existing ranked candidates are
	// then retained in their model order, excluding duplicates.
	result := make([]*types.SearchResult, 0, len(promoted)+len(ranked))
	result = append(result, promoted...)
	for _, item := range ranked {
		if item == nil {
			continue
		}
		key := searchResultIdentity(item)
		if _, exists := seen[key]; exists {
			continue
		}
		result = append(result, item)
		seen[key] = struct{}{}
	}
	return result
}

// PromoteExplicitCoverageMatches preserves one source candidate for each rare,
// explicitly named topic in a multi-aspect query. A single reranker score can
// otherwise favor two passages about one aspect and discard a different aspect
// completely. The policy is bounded, uses only already-authorized retrieval
// candidates, and activates only for list/comparison-shaped requests.
func PromoteExplicitCoverageMatches(
	query string,
	candidates []*types.SearchResult,
	ranked []*types.SearchResult,
) []*types.SearchResult {
	if !containsFoldAny(query, multiAspectQueryMarkers) || len(candidates) == 0 {
		return ranked
	}

	queryTokens := searchutil.TokenizeSimple(query)
	tokens := make([]coverageTokenCandidate, 0, len(queryTokens))
	tokenIndex := make(map[string]int, len(queryTokens))
	maxDocumentFrequency := (len(candidates) + 1) / 2
	if maxDocumentFrequency < 4 {
		maxDocumentFrequency = 4
	}
	addToken := func(token string, explicit bool) {
		token = strings.ToLower(strings.TrimSpace(token))
		if !usefulCoverageToken(token) {
			return
		}
		df := 0
		for _, candidate := range candidates {
			if candidateContainsToken(candidate, token) {
				df++
			}
		}
		// Named topics commonly occur in a definition, a condition paragraph and
		// a summary/list paragraph. Requiring df<=2 discarded exactly those
		// multi-aspect topics and instead promoted rare instruction words such as
		// “具体” or “比较”. Keep bounded, moderately selective terms and discard
		// shorter terms when the query also contains a more specific one.
		if df == 0 || (!explicit && df > maxDocumentFrequency) {
			return
		}
		if index, exists := tokenIndex[token]; exists {
			tokens[index].explicit = tokens[index].explicit || explicit
			return
		}
		tokenIndex[token] = len(tokens)
		tokens = append(tokens, coverageTokenCandidate{token: token, df: df, explicit: explicit})
	}
	// Jieba intentionally emits fine-grained search tokens.  That is useful for
	// recall but can split an explicitly named compound (for example a method or
	// product name) into common words whose document frequency is too high for
	// coverage promotion.  Recover the user's comma/conjunction-delimited list
	// before adding the segmented tokens, and allow those exact phrases through
	// the document-frequency guard.
	for _, token := range explicitComparisonItems(query) {
		addToken(token, true)
	}
	for token := range queryTokens {
		addToken(token, false)
	}
	tokens = removeCoveredShortTokens(tokens)
	if len(tokens) < 2 {
		return ranked
	}
	sort.SliceStable(tokens, func(i, j int) bool {
		if tokens[i].explicit != tokens[j].explicit {
			return tokens[i].explicit
		}
		leftRunes, rightRunes := len([]rune(tokens[i].token)), len([]rune(tokens[j].token))
		if leftRunes != rightRunes {
			return leftRunes > rightRunes
		}
		if tokens[i].df != tokens[j].df {
			return tokens[i].df < tokens[j].df
		}
		return tokens[i].token < tokens[j].token
	})

	const promotionLimit = 6
	seen := make(map[string]struct{}, len(ranked))
	for _, item := range ranked {
		if item != nil {
			seen[searchResultIdentity(item)] = struct{}{}
		}
	}
	promoted := make([]*types.SearchResult, 0, promotionLimit)
	coveredCandidates := make(map[string]struct{}, promotionLimit)
	promotedIdentities := make(map[string]struct{}, promotionLimit)
	for _, item := range tokens {
		var best *types.SearchResult
		bestScore := noCoverageCandidateScore
		for _, candidate := range candidates {
			if candidate == nil {
				continue
			}
			identity := searchResultIdentity(candidate)
			if _, used := coveredCandidates[identity]; used {
				continue
			}
			score := coverageCandidateScore(query, candidate, item.token)
			if score == noCoverageCandidateScore {
				continue
			}
			if best == nil || score > bestScore || (score == bestScore && candidate.Score > best.Score) {
				best = candidate
				bestScore = score
			}
		}
		if best == nil {
			continue
		}
		identity := searchResultIdentity(best)
		coveredCandidates[identity] = struct{}{}
		if _, exists := seen[identity]; exists {
			continue
		}
		best.Metadata = ensureSearchMetadata(best.Metadata)
		best.Metadata["explicit_query_topic"] = item.token
		best.Metadata["pre_coverage_promotion_score"] = strconv.FormatFloat(best.Score, 'f', 6, 64)
		best.Score = 0.999
		promoted = append(promoted, best)
		seen[identity] = struct{}{}
		promotedIdentities[identity] = struct{}{}
		if len(promoted) == promotionLimit {
			break
		}
	}
	if len(promoted) == 0 {
		return ranked
	}
	result := make([]*types.SearchResult, 0, len(promoted)+len(ranked))
	result = append(result, promoted...)
	for _, item := range ranked {
		if item == nil {
			continue
		}
		identity := searchResultIdentity(item)
		if _, promotedHere := promotedIdentities[identity]; promotedHere {
			continue
		}
		result = append(result, item)
	}
	return result
}

// explicitComparisonItems extracts a bounded list immediately following a
// comparison/list marker.  It is intentionally syntactic rather than
// domain-specific: the same logic covers named procurement methods, document
// types, products, policies, or any other user-provided alternatives.
func explicitComparisonItems(query string) []string {
	value := strings.ToLower(strings.TrimSpace(query))
	if value == "" {
		return nil
	}
	start := -1
	markerLen := 0
	for _, marker := range []string{"分别说明", "请比较", "比较", "对比", "compare", "list"} {
		if index := strings.Index(value, marker); index >= 0 && (start < 0 || index < start) {
			start = index
			markerLen = len(marker)
		}
	}
	if start < 0 {
		return nil
	}
	tail := strings.TrimSpace(value[start+markerLen:])
	for _, endMarker := range []string{
		"的定义", "的适配", "的适用", "的条件", "的要求", "分别", "每种", "每个",
		"逐项", "逐一", "。", "！", "？", "\n", ". ", "? ", "! ",
	} {
		if index := strings.Index(tail, endMarker); index > 0 {
			tail = tail[:index]
		}
	}
	tail = strings.Trim(tail, " ：:，,、；;。.!！?？()（）[]【】")
	if tail == "" {
		return nil
	}

	parts := explicitListSeparatorPattern.Split(tail, -1)
	seen := make(map[string]struct{}, len(parts))
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(strings.Trim(part, " ：:，,、；;。.!！?？()（）[]【】"))
		part = strings.TrimPrefix(part, "以下")
		part = strings.TrimPrefix(part, "下列")
		part = strings.TrimPrefix(part, "这些")
		part = strings.TrimPrefix(part, "the ")
		part = strings.TrimSpace(part)
		if runeCount := len([]rune(part)); runeCount < 2 || runeCount > 24 {
			continue
		}
		if _, exists := seen[part]; exists {
			continue
		}
		seen[part] = struct{}{}
		items = append(items, part)
	}
	if len(items) < 2 || len(items) > 8 {
		return nil
	}
	return items
}

func removeCoveredShortTokens(tokens []coverageTokenCandidate) []coverageTokenCandidate {
	result := make([]coverageTokenCandidate, 0, len(tokens))
	for _, candidate := range tokens {
		covered := false
		for _, other := range tokens {
			if candidate.token == other.token {
				continue
			}
			if len([]rune(other.token)) > len([]rune(candidate.token)) &&
				strings.Contains(other.token, candidate.token) {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, candidate)
		}
	}
	return result
}

func coverageCandidateScore(query string, candidate *types.SearchResult, token string) int {
	index := tokenOccurrenceIndex(candidate, token)
	if index < 0 {
		return noCoverageCandidateScore
	}
	content := strings.ToLower(candidate.KnowledgeTitle + "\n" + candidate.Content)
	score := -index
	// Definition-shaped requests should prefer the fragment that actually
	// defines the named topic, rather than an operational paragraph that merely
	// mentions it. Applicability-shaped requests similarly prefer explicit
	// condition language. These are generic textual cues, not domain labels.
	if containsFoldAny(query, []string{"定义", "是什么", "what is", "define"}) &&
		containsNear(content, token, []string{"是指", "指的是", " means ", " is "}, 24) {
		score += 10000
	}
	if containsFoldAny(query, []string{"适用", "适配", "条件", "要求", "重点", "风险", "applicable", "condition"}) &&
		containsNear(content, token, []string{"适宜", "适用", "条件", "要求", "符合"}, 240) {
		score += 5000
	}
	if structuralIdentifierPattern.MatchString(candidate.Content) {
		score += 500
	}
	return score
}

func containsNear(content, token string, markers []string, radius int) bool {
	index := strings.Index(content, token)
	if index < 0 {
		return false
	}
	start := index - radius
	if start < 0 {
		start = 0
	}
	end := index + len(token) + radius
	if end > len(content) {
		end = len(content)
	}
	window := content[start:end]
	return containsFoldAny(window, markers)
}

func usefulCoverageToken(token string) bool {
	if _, stop := lexicalCoverageStopwords[token]; stop {
		return false
	}
	runeCount := len([]rune(token))
	if runeCount < 2 || runeCount > 24 {
		return false
	}
	return true
}

func candidateContainsToken(candidate *types.SearchResult, token string) bool {
	return tokenOccurrenceIndex(candidate, token) >= 0
}

func tokenOccurrenceIndex(candidate *types.SearchResult, token string) int {
	if candidate == nil {
		return -1
	}
	value := strings.ToLower(candidate.KnowledgeTitle + "\n" + candidate.Content)
	return strings.Index(value, token)
}

func containsFoldAny(value string, markers []string) bool {
	value = strings.ToLower(value)
	for _, marker := range markers {
		if strings.Contains(value, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func structuralIdentifiers(value string) []string {
	matches := structuralIdentifierPattern.FindAllString(value, -1)
	seen := make(map[string]struct{}, len(matches))
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		identifier := normalizeStructuralText(match)
		if identifier == "" {
			continue
		}
		if _, exists := seen[identifier]; exists {
			continue
		}
		seen[identifier] = struct{}{}
		result = append(result, identifier)
	}
	return result
}

func containsStructuralIdentifier(content string, identifiers []string) bool {
	content = normalizeStructuralText(content)
	for _, identifier := range identifiers {
		if strings.Contains(content, identifier) {
			return true
		}
	}
	return false
}

func normalizeStructuralText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		if r >= '０' && r <= '９' {
			return '0' + (r - '０')
		}
		return r
	}, value)
}

func searchResultIdentity(result *types.SearchResult) string {
	if result.ID != "" {
		return "id:" + result.ID
	}
	return strings.Join([]string{
		result.KnowledgeID,
		result.KnowledgeTitle,
		strconv.Itoa(result.ChunkIndex),
		result.Content,
	}, "\x00")
}

func ensureSearchMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return make(map[string]string)
	}
	return metadata
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
