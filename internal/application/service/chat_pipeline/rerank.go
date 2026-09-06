package chatpipeline

import (
	"context"
	"fmt"
	"github.com/Tencent/WeKnora/internal/custom/modules/chatretrieval"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginRerank implements reranking functionality for chat pipeline
type PluginRerank struct {
	modelService interfaces.ModelService // Service to access rerank models
}

// NewPluginRerank creates a new rerank plugin instance
func NewPluginRerank(eventManager *EventManager, modelService interfaces.ModelService) *PluginRerank {
	res := &PluginRerank{
		modelService: modelService,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginRerank) ActivationEvents() []types.EventType {
	return []types.EventType{types.CHUNK_RERANK}
}

// OnEvent handles reranking events in the chat pipeline
func (p *PluginRerank) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	if !chatManage.NeedsRetrieval() {
		return next()
	}
	pipelineInfo(ctx, "Rerank", "input", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"candidate_cnt":  len(chatManage.SearchResult),
		"rerank_model":   chatManage.RerankModelID,
		"rerank_thresh":  chatManage.RerankThreshold,
		"rewrite_query":  chatManage.RewriteQuery,
		"evidence_query": chatManage.RetrievalQuery(),
	})
	if len(chatManage.SearchResult) == 0 {
		pipelineInfo(ctx, "Rerank", "skip", map[string]interface{}{
			"reason": "empty_search_result",
		})
		return next()
	}
	if chatManage.RerankModelID == "" {
		pipelineWarn(ctx, "Rerank", "skip", map[string]interface{}{
			"reason": "empty_model_id",
		})
		return next()
	}

	// Get rerank model from service
	rerankModel, err := p.modelService.GetRerankModel(ctx, chatManage.RerankModelID)
	if err != nil {
		pipelineError(ctx, "Rerank", "get_model", map[string]interface{}{
			"model_id": chatManage.RerankModelID,
			"error":    err.Error(),
		})
		return ErrGetRerankModel.WithError(err)
	}

	// Prepare passages for reranking (excluding DirectLoad results)
	var passages []string
	var candidatesToRerank []*types.SearchResult
	var directLoadResults []*types.SearchResult

	for _, result := range chatManage.SearchResult {
		if result.MatchType == types.MatchTypeDirectLoad {
			directLoadResults = append(directLoadResults, result)
			pipelineInfo(ctx, "Rerank", "direct_load_skip", map[string]interface{}{
				"chunk_id": result.ID,
			})
			continue
		}
		passage := getEnrichedPassage(ctx, result)
		if strings.TrimSpace(passage) == "" {
			pipelineInfo(ctx, "Rerank", "empty_passage_skip", map[string]interface{}{
				"chunk_id": result.ID,
			})
			continue
		}
		passages = append(passages, passage)
		candidatesToRerank = append(candidatesToRerank, result)
	}

	mgr := langfuse.GetManager()
	var passagesPreview interface{}
	if mgr.EnabledFor(ctx) {
		passagesPreview = langfuse.SummarizePassagePreviews(candidatesToRerank, passages, 25)
	}
	rerankCtx, rerankSpan := mgr.StartSpan(ctx, langfuse.SpanOptions{
		Name: "rerank",
		Input: map[string]interface{}{
			"query":             chatManage.RetrievalQuery(),
			"candidate_count":   len(candidatesToRerank),
			"direct_load_count": len(directLoadResults),
			"rerank_model_id":   chatManage.RerankModelID,
			"threshold":         chatManage.RerankThreshold,
			"rerank_top_k":      chatManage.RerankTopK,
			"faq_priority":      chatManage.FAQPriorityEnabled,
			"passages_preview":  passagesPreview,
		},
		Metadata: map[string]interface{}{
			"session_id": chatManage.SessionID,
		},
	})
	ctx = rerankCtx
	spanOutput := map[string]interface{}{}
	var spanErr error
	defer func() {
		rerankSpan.Finish(spanOutput, nil, spanErr)
	}()

	pipelineInfo(ctx, "Rerank", "build_passages", map[string]interface{}{
		"total_cnt":     len(chatManage.SearchResult),
		"candidate_cnt": len(candidatesToRerank),
		"direct_cnt":    len(directLoadResults),
	})

	var rerankResp []rerank.RankResult
	var rawRerankResp []rerank.RankResult

	// Only call rerank model if there are candidates
	if len(candidatesToRerank) > 0 {
		// One configured rerank request is the relevance gate. Retrying with a
		// lower threshold both adds latency and admits evidence that the owner
		// explicitly configured the model to reject.
		var rerankErr error
		rerankResp, rerankErr = p.rerank(ctx, chatManage, rerankModel, chatManage.RetrievalQuery(), passages, candidatesToRerank)

		if rerankErr != nil {
			pipelineError(ctx, "Rerank", "api_error", map[string]interface{}{
				"error":         rerankErr.Error(),
				"candidate_cnt": len(candidatesToRerank),
			})
			spanOutput = map[string]interface{}{
				"stage":           "api_error",
				"candidate_count": len(candidatesToRerank),
				"error":           rerankErr.Error(),
			}
			spanErr = rerankErr
			return ErrRerank.WithError(rerankErr)
		}
		rawRerankResp = append([]rerank.RankResult(nil), rerankResp...)
	}

	pipelineInfo(ctx, "Rerank", "model_response", map[string]interface{}{
		"result_cnt": len(rerankResp),
	})

	logRerankInputScoreSample(ctx, chatManage.SearchResult)

	for i := range chatManage.SearchResult {
		chatManage.SearchResult[i].Metadata = ensureMetadata(chatManage.SearchResult[i].Metadata)
	}
	reranked := make([]*types.SearchResult, 0, len(rerankResp)+len(directLoadResults))

	// Process reranked results
	for _, rr := range rerankResp {
		if rr.Index < 0 || rr.Index >= len(candidatesToRerank) {
			continue
		}
		sr := candidatesToRerank[rr.Index]
		base := sr.Score
		sr.Metadata["base_score"] = fmt.Sprintf("%.4f", base)
		modelScore := rr.RelevanceScore
		sr.Metadata["model_score"] = fmt.Sprintf("%.4f", modelScore)
		sr.Score = modelScore

		reranked = append(reranked, sr)
	}

	// Process direct load results (bypass rerank model, assume high relevance)
	for _, sr := range directLoadResults {
		base := sr.Score
		sr.Metadata["base_score"] = fmt.Sprintf("%.4f", base)
		modelScore := 1.0
		sr.Metadata["model_score"] = fmt.Sprintf("%.4f", modelScore)
		// Assign high model score for direct load items
		sr.Score = modelScore
		reranked = append(reranked, sr)
	}
	budget := chatretrieval.ResolveBudget(chatManage.EmbeddingTopK, chatManage.RerankTopK, chatManage.RetrievalBudget)
	final := chatretrieval.Select(reranked, budget.Fusion, 0)
	chatManage.RerankResult = final

	// Log the model relevance used by the shared selector.
	topN := min(3, len(reranked))
	for i := 0; i < topN; i++ {
		pipelineInfo(ctx, "Rerank", "relevance_top", map[string]interface{}{
			"rank":        i + 1,
			"chunk_id":    reranked[i].ID,
			"base_score":  reranked[i].Metadata["base_score"],
			"final_score": fmt.Sprintf("%.4f", reranked[i].Score),
		})
	}

	if len(chatManage.RerankResult) == 0 {
		pipelineWarn(ctx, "Rerank", "output", map[string]interface{}{
			"filtered_cnt": 0,
		})
		spanOutput = buildRerankSpanOutput(
			candidatesToRerank,
			passages,
			directLoadResults,
			rawRerankResp,
			reranked,
			nil,
			chatManage,
		)
		return ErrSearchNothing
	}

	spanOutput = buildRerankSpanOutput(
		candidatesToRerank,
		passages,
		directLoadResults,
		rawRerankResp,
		reranked,
		chatManage.RerankResult,
		chatManage,
	)
	pipelineInfo(ctx, "Rerank", "output", map[string]interface{}{
		"filtered_cnt": len(chatManage.RerankResult),
	})
	return next()
}

func buildRerankSpanOutput(
	candidates []*types.SearchResult,
	passages []string,
	directLoad []*types.SearchResult,
	modelScores []rerank.RankResult,
	composite []*types.SearchResult,
	final []*types.SearchResult,
	chatManage *types.ChatManage,
) map[string]interface{} {
	modelRows := make([]map[string]interface{}, 0, len(modelScores))
	for i, rr := range modelScores {
		row := map[string]interface{}{
			"rank":        i + 1,
			"index":       rr.Index,
			"model_score": rr.RelevanceScore,
		}
		if rr.Index >= 0 && rr.Index < len(candidates) {
			row["chunk_id"] = candidates[rr.Index].ID
			row["knowledge_id"] = candidates[rr.Index].KnowledgeID
			row["knowledge_title"] = candidates[rr.Index].KnowledgeTitle
			row["match_type"] = candidates[rr.Index].MatchType
			row["retrieval_score"] = candidates[rr.Index].Score
			if rr.Index < len(passages) {
				row["preview"] = langfuse.TruncateRunes(passages[rr.Index], 160)
			}
		}
		modelRows = append(modelRows, row)
	}

	out := map[string]interface{}{
		"candidate_count":    len(candidates),
		"direct_load_count":  len(directLoad),
		"model_result_count": len(modelScores),
		"composite_count":    len(composite),
		"final_count":        len(final),
		"threshold":          chatManage.RerankThreshold,
		"rerank_top_k":       chatManage.RerankTopK,
		"model_scores":       langfuse.SummarizeRankScores(modelRows, 50),
		"composite_results":  langfuse.SummarizeSearchResults(composite, 25),
		"final_results":      langfuse.SummarizeSearchResults(final, 25),
	}
	if len(modelScores) > 50 {
		out["model_scores_truncated"] = len(modelScores) - 50
	}
	return out
}

// rerank performs the actual reranking operation with given query and passages
func (p *PluginRerank) rerank(ctx context.Context,
	chatManage *types.ChatManage, rerankModel rerank.Reranker, query string, passages []string,
	candidates []*types.SearchResult,
) ([]rerank.RankResult, error) {
	pipelineInfo(ctx, "Rerank", "model_call", map[string]interface{}{
		"query_variant": query,
		"passages":      len(passages),
	})

	// Filter out empty or whitespace-only passages before sending to the API
	var cleanPassages []string
	var cleanCandidates []*types.SearchResult
	for i, p := range passages {
		if strings.TrimSpace(p) != "" {
			cleanPassages = append(cleanPassages, p)
			if i < len(candidates) {
				cleanCandidates = append(cleanCandidates, candidates[i])
			}
		}
	}
	if len(cleanPassages) == 0 {
		pipelineInfo(ctx, "Rerank", "model_call_skip", map[string]interface{}{
			"reason": "all_passages_empty",
		})
		return nil, nil
	}
	passages = cleanPassages
	candidates = cleanCandidates

	budget := chatretrieval.ResolveBudget(chatManage.EmbeddingTopK, chatManage.RerankTopK, chatManage.RetrievalBudget)
	budget.EvidenceCount = budget.Fusion
	budget.EvidenceTokens = 0
	queries := chatManage.RetrievalQueries()
	if len(queries) == 0 {
		queries = []string{query}
	}
	ranked, err := chatretrieval.Rank(ctx, queries, candidates, func(ctx context.Context, q string, docs []string) (map[int]float64, error) {
		rows, err := rerankModel.Rerank(ctx, q, docs)
		scores := map[int]float64{}
		for _, row := range rows {
			scores[row.Index] = row.RelevanceScore
		}
		return scores, err
	}, chatManage.RerankThreshold, budget)
	var rerankResp []rerank.RankResult
	indexes := map[string]int{}
	for i, c := range candidates {
		indexes[chatretrieval.Identity(c)] = i
	}
	for _, c := range ranked {
		index := indexes[chatretrieval.Identity(c)]
		candidates[index].QueryScores = c.QueryScores
		rerankResp = append(rerankResp, rerank.RankResult{Index: index, RelevanceScore: c.Score})
	}

	if err != nil {
		pipelineError(ctx, "Rerank", "model_call", map[string]interface{}{
			"query_variant": query,
			"error":         err.Error(),
		})
		return nil, err
	}

	// Log top scores for debugging
	pipelineInfo(ctx, "Rerank", "threshold", map[string]interface{}{
		"threshold": chatManage.RerankThreshold,
	})
	logged := min(5, len(rerankResp))
	for i := range logged {
		if rerankResp[i].Index >= 0 && rerankResp[i].Index < len(candidates) {
			pipelineInfo(ctx, "Rerank", "top_score", map[string]interface{}{
				"rank":        i + 1,
				"score":       rerankResp[i].RelevanceScore,
				"chunk_id":    candidates[rerankResp[i].Index].ID,
				"match_type":  candidates[rerankResp[i].Index].MatchType,
				"chunk_type":  candidates[rerankResp[i].Index].ChunkType,
				"content_len": len(candidates[rerankResp[i].Index].Content),
			})
		}
	}
	if len(rerankResp) > logged {
		pipelineInfo(ctx, "Rerank", "top_score_summary", map[string]interface{}{
			"total":     len(rerankResp),
			"logged":    logged,
			"truncated": len(rerankResp) - logged,
		})
	}

	return rerankResp, nil
}

// ensureMetadata ensures the metadata is not nil
func ensureMetadata(m map[string]string) map[string]string {
	if m == nil {
		return make(map[string]string)
	}
	return m
}

// getEnrichedPassage 合并Content、ImageInfo和GeneratedQuestions的文本内容
func getEnrichedPassage(ctx context.Context, result *types.SearchResult) string {
	return chatretrieval.Passage(result)
}

func logRerankInputScoreSample(ctx context.Context, results []*types.SearchResult) {
	const maxLogRows = 8
	limit := min(maxLogRows, len(results))
	for i := 0; i < limit; i++ {
		sr := results[i]
		pipelineInfo(ctx, "Rerank", "input_score", map[string]interface{}{
			"index":      i,
			"chunk_id":   sr.ID,
			"score":      fmt.Sprintf("%.4f", sr.Score),
			"match_type": sr.MatchType,
		})
	}
	if len(results) > limit {
		pipelineInfo(ctx, "Rerank", "input_score_summary", map[string]interface{}{
			"total":     len(results),
			"logged":    limit,
			"truncated": len(results) - limit,
		})
	}
}
