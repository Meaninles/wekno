package rerank

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Every adapter sees bounded semantic windows. Scores are projected back to
// the original evidence identity; a passage tail is never silently discarded.
type windowedReranker struct {
	Reranker
	maxTokens int
}

func (r *windowedReranker) Rerank(ctx context.Context, query string, documents []string) ([]RankResult, error) {
	// Without the provider's tokenizer, UTF-8 bytes form a conservative input
	// bound. A language-average character ratio can silently drop CJK tails.
	remaining := r.maxTokens - len(query) - 64
	if remaining < 32 {
		return nil, fmt.Errorf("rerank query exceeds configured input budget %d", r.maxTokens)
	}
	var windows []string
	var owners []int
	for i, doc := range documents {
		for _, window := range passageWindows(doc, remaining) {
			windows = append(windows, window)
			owners = append(owners, i)
		}
	}
	best := make(map[int]RankResult, len(documents))
	// Bounded batches avoid oversized provider requests. The existing model
	// admission layer owns global concurrency; no unbounded per-window fanout.
	for start := 0; start < len(windows); start += 64 {
		end := min(start+64, len(windows))
		scores, err := r.Reranker.Rerank(ctx, query, windows[start:end])
		if err != nil {
			return nil, err
		}
		for _, score := range scores {
			if score.Index < 0 || score.Index >= end-start {
				return nil, fmt.Errorf("reranker returned invalid window index %d", score.Index)
			}
			index := owners[start+score.Index]
			previous, ok := best[index]
			if !ok || score.RelevanceScore > previous.RelevanceScore {
				score.Index = index
				score.Document = DocumentInfo{Text: documents[index]}
				best[index] = score
			}
		}
	}
	out := make([]RankResult, 0, len(best))
	for _, score := range best {
		out = append(out, score)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RelevanceScore != out[j].RelevanceScore {
			return out[i].RelevanceScore > out[j].RelevanceScore
		}
		return out[i].Index < out[j].Index
	})
	return out, nil
}

func passageWindows(text string, budget int) []string {
	runes := []rune(text)
	if len(text) <= budget {
		return []string{text}
	}
	var result []string
	for start := 0; start < len(runes); {
		end, bytes := start, 0
		for end < len(runes) && bytes+utf8.RuneLen(runes[end]) <= budget {
			bytes += utf8.RuneLen(runes[end])
			end++
		}
		if end == start {
			end++
		}
		span := end - start
		if end < len(runes) {
			for i := end - 1; i > start+span/2; i-- {
				if runes[i] == '\n' {
					end = i + 1
					break
				}
			}
		}
		result = append(result, strings.TrimSpace(string(runes[start:end])))
		if end == len(runes) {
			break
		}
		// Small overlap protects a sentence/row crossing the hard boundary.
		start = max(start+1, end-min(64, span/8))
	}
	return result
}
