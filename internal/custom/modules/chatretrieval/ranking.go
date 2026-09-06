package chatretrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
)

type Budget struct{ Candidates, Fusion, EvidenceCount, EvidenceTokens int }

func ResolveBudget(recall, final int, configured ...types.RetrievalBudget) Budget {
	if recall <= 0 {
		recall = 10
	}
	if final <= 0 {
		final = 5
	}
	budget := Budget{Candidates: min(256, max(64, recall*8)), Fusion: min(128, max(32, recall*4, final)), EvidenceTokens: 8192}
	if len(configured) > 0 {
		c := configured[0]
		if c.CandidateCount > 0 {
			budget.Candidates = min(500, c.CandidateCount)
		}
		if c.FusionCount > 0 {
			budget.Fusion = min(500, c.FusionCount)
		}
		if c.EvidenceTokens > 0 {
			budget.EvidenceTokens = min(64000, c.EvidenceTokens)
		}
	}
	// Final evidence is bounded by its serialized context cost. Applying the
	// old small rerank_top_k here discarded independently relevant conditions
	// even when the configured context budget still had room.
	budget.EvidenceCount = budget.Fusion
	return budget
}

func Identity(r *types.SearchResult) string {
	if r.ID != "" {
		return r.KnowledgeBaseID + ":" + r.KnowledgeID + ":" + r.ID
	}
	return fmt.Sprintf("%s:%s:%d:%d:%s", r.KnowledgeBaseID, r.KnowledgeID, r.StartAt, r.EndAt, r.Content)
}

// Deduplicate preserves all query ownership; choosing one winning query before
// scoring makes multi-question coverage impossible to recover downstream.
func Deduplicate(in []*types.SearchResult) []*types.SearchResult {
	byID := map[string]*types.SearchResult{}
	var out []*types.SearchResult
	for _, r := range in {
		if r == nil {
			continue
		}
		key := Identity(r)
		if previous := byID[key]; previous != nil {
			for _, q := range r.MatchedQueries {
				if !slices.Contains(previous.MatchedQueries, q) {
					previous.MatchedQueries = append(previous.MatchedQueries, q)
				}
			}
			for q, score := range r.QueryScores {
				if previous.QueryScores == nil {
					previous.QueryScores = map[string]float64{}
				}
				old, exists := previous.QueryScores[q]
				if !exists || score > old {
					previous.QueryScores[q] = score
				}
			}
			previous.Score = max(previous.Score, r.Score)
			continue
		}
		copy := *r
		copy.MatchedQueries = append([]string(nil), r.MatchedQueries...)
		copy.QueryScores = map[string]float64{}
		for q, score := range r.QueryScores {
			copy.QueryScores[q] = score
		}
		byID[key] = &copy
		out = append(out, &copy)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return Identity(out[i]) < Identity(out[j])
	})
	return out
}

// Select greedily maximizes evidence relevance plus additional query coverage.
// It has no document/domain quotas. Each score belongs to an actual query;
// unrelated results do not acquire coverage merely by sharing the pool.
func Select(in []*types.SearchResult, count int, tokenBudget int) []*types.SearchResult {
	if count <= 0 {
		count = len(in)
	}
	covered := map[string]float64{}
	used := map[string]bool{}
	var out []*types.SearchResult
	tokens := 0
	for len(out) < count {
		bestIndex := -1
		bestGain := math.Inf(-1)
		for i, r := range in {
			if r == nil || used[Identity(r)] {
				continue
			}
			cost := evidenceTokenCost(EvidenceText(r)) + 32 // registered citation handle
			if tokenBudget > 0 && tokens+cost > tokenBudget {
				continue
			}
			gain := r.Score
			if len(r.QueryScores) > 0 {
				for q, score := range r.QueryScores {
					gain += max(0, score-covered[q])
				}
			} else {
				for _, q := range r.MatchedQueries {
					gain += max(0, r.Score-covered[q])
				}
			}
			if gain > bestGain || (gain == bestGain && bestIndex >= 0 && Identity(r) < Identity(in[bestIndex])) {
				bestIndex = i
				bestGain = gain
			}
		}
		if bestIndex < 0 {
			break
		}
		r := in[bestIndex]
		out = append(out, r)
		used[Identity(r)] = true
		tokens += evidenceTokenCost(EvidenceText(r)) + 32
		if len(r.QueryScores) > 0 {
			for q, score := range r.QueryScores {
				covered[q] = max(covered[q], score)
			}
		} else {
			for _, q := range r.MatchedQueries {
				covered[q] = max(covered[q], r.Score)
			}
		}
	}
	return out
}

type Scorer func(context.Context, string, []string) (map[int]float64, error)

// Rank is the common native/pipeline/SDK retrieval ranking boundary. Scorers
// report model relevance; query coverage and evidence budgets are owned here.
func Rank(ctx context.Context, queries []string, in []*types.SearchResult, score Scorer, threshold float64, budget Budget) ([]*types.SearchResult, error) {
	pool := Select(Deduplicate(in), budget.Fusion, 0)
	if len(pool) == 0 {
		return nil, nil
	}
	if score == nil {
		return Select(pool, budget.EvidenceCount, budget.EvidenceTokens), nil
	}
	passages := make([]string, len(pool))
	for i, r := range pool {
		passages[i] = Passage(r)
		pool[i].QueryScores = map[string]float64{}
		pool[i].Score = math.Inf(-1)
	}
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	seenQueries := map[string]bool{}
	for _, query := range queries {
		if strings.TrimSpace(query) == "" || seenQueries[query] {
			continue
		}
		seenQueries[query] = true
		wg.Add(1)
		go func(q string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				failures = append(failures, ctx.Err())
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			scores, err := score(ctx, q, passages)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			for index, value := range scores {
				if index < 0 || index >= len(pool) {
					failures = append(failures, fmt.Errorf("invalid rerank index %d", index))
					continue
				}
				if math.IsNaN(value) || math.IsInf(value, 0) {
					failures = append(failures, fmt.Errorf("invalid rerank score"))
					continue
				}
				pool[index].QueryScores[q] = value
				pool[index].Score = max(pool[index].Score, value)
			}
		}(query)
	}
	wg.Wait()
	if len(seenQueries) == 0 {
		return nil, fmt.Errorf("rerank requires a non-empty query")
	}
	if len(failures) > 0 {
		return nil, fmt.Errorf("rerank failed: %w", errors.Join(failures...))
	}
	eligible := pool[:0]
	for _, r := range pool {
		if r.Score >= threshold {
			eligible = append(eligible, r)
		}
	}
	return Select(eligible, budget.EvidenceCount, budget.EvidenceTokens), nil
}

// Passage keeps source structure intact and labels auxiliary retrieval hints.
// It is shared across QA paths; markdown stripping used to remove table/URL
// distinctions before the reranker could see them.
func Passage(r *types.SearchResult) string {
	if r == nil {
		return ""
	}
	parts := []string{}
	seen := map[string]bool{}
	add := func(label, text string) {
		text = strings.TrimSpace(text)
		if text == "" || seen[text] {
			return
		}
		for previous := range seen {
			if strings.Contains(previous, text) {
				return
			}
		}
		seen[text] = true
		parts = append(parts, label+text)
	}
	add("Document: ", r.KnowledgeTitle)
	content := r.EvidenceContent
	if strings.TrimSpace(content) == "" {
		content = r.Content
	}
	add("", content)

	var images []types.ImageInfo
	if json.Unmarshal([]byte(r.ImageInfo), &images) == nil {
		for _, im := range images {
			add("Image OCR: ", im.OCRText)
			add("Image description: ", im.Caption)
		}
	}
	var faq types.FAQChunkMetadata
	if json.Unmarshal(r.ChunkMetadata, &faq) == nil {
		add("Question: ", faq.StandardQuestion)
		for _, a := range faq.Answers {
			add("Answer: ", a)
		}
	}
	return strings.Join(parts, "\n\n")
}

func evidenceTokenCost(value string) int {
	ascii, other := 0, 0
	for _, r := range value {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}
