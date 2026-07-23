package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// loadGenerationChunkStrata selects deterministic, evenly distributed chunks
// from one immutable physical-split generation. It scans in bounded pages so
// post-processing a very large logical document never materializes every
// chunk in memory merely to choose a representative sample.
func loadGenerationChunkStrata(
	ctx context.Context,
	manager *documentsplit.Manager,
	tenantID uint64,
	knowledgeID, generation string,
	chunkTypes []types.ChunkType,
	maximumStrata int64,
) ([]*types.Chunk, int64, error) {
	if manager == nil {
		return nil, 0, errors.New("logical document sampler is unavailable")
	}
	if maximumStrata <= 0 {
		return nil, 0, errors.New("logical document sampler requires a positive stratum limit")
	}

	total, err := manager.CountGenerationChunks(
		ctx, tenantID, knowledgeID, generation, chunkTypes,
	)
	if err != nil || total == 0 {
		return nil, total, err
	}
	strata := min(total, maximumStrata)
	targets := make([]int64, 0, strata)
	if strata == 1 {
		targets = append(targets, 0)
	} else {
		for index := int64(0); index < strata; index++ {
			targets = append(targets, index*(total-1)/(strata-1))
		}
	}

	selected := make([]*types.Chunk, 0, len(targets))
	cursor := documentsplit.GenerationChunkCursor{ChunkIndex: -1}
	ordinal := int64(0)
	targetIndex := 0
	for targetIndex < len(targets) {
		page, listErr := manager.ListGenerationChunksByTypeAfter(
			ctx, tenantID, knowledgeID, generation, chunkTypes, cursor, 500,
		)
		if listErr != nil {
			return nil, total, listErr
		}
		if len(page) == 0 {
			break
		}
		for _, chunk := range page {
			for targetIndex < len(targets) && targets[targetIndex] == ordinal {
				selected = append(selected, chunk)
				targetIndex++
			}
			ordinal++
			cursor = documentsplit.GenerationChunkCursor{
				ChunkIndex: chunk.ChunkIndex,
				ChunkID:    chunk.ID,
			}
		}
	}
	if targetIndex != len(targets) || ordinal != total {
		return nil, total, errors.New("logical document changed while selecting strata")
	}
	return selected, total, nil
}

func (s *wikiIngestService) loadWikiLogicalChunks(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, generation string,
) ([]*types.Chunk, int64, error) {
	loadOrdinary := func() ([]*types.Chunk, int64, error) {
		chunks, err := s.chunkRepo.ListChunksByKnowledgeID(ctx, tenantID, knowledgeID)
		if err != nil {
			return nil, 0, err
		}
		filtered := filterTextChunks(chunks)
		return filtered, int64(len(filtered)), nil
	}
	if s.splitManager == nil || strings.TrimSpace(generation) == "" {
		return loadOrdinary()
	}
	if _, err := s.splitManager.GetPlanForGeneration(
		ctx, tenantID, knowledgeID, generation,
	); errors.Is(err, gorm.ErrRecordNotFound) {
		return loadOrdinary()
	} else if err != nil {
		return nil, 0, err
	}

	// A 32K-rune Wiki input can usefully carry roughly 48 normal chunks.
	// Sampling the full logical coordinate range is materially better than
	// retaining only the first physical part, while each chosen chunk still
	// has enough local content for entity and concept extraction.
	return loadGenerationChunkStrata(
		ctx, s.splitManager, tenantID, knowledgeID, generation,
		[]types.ChunkType{types.ChunkTypeText}, 48,
	)
}

// reconstructWikiLogicalContent keeps the ordinary-document path byte-for-byte
// compatible. For a physical split generation, each sampled block is rebuilt
// independently, labelled with its original logical coordinate, and given a
// fair share of the fixed Wiki context budget. This avoids accidental overlap
// stitching across distant samples and prevents one large early worksheet or
// chapter from consuming the whole prompt.
func (s *wikiIngestService) reconstructWikiLogicalContent(
	ctx context.Context,
	tenantID uint64,
	chunks []*types.Chunk,
	logicalChunkCount int64,
) string {
	if len(chunks) == 0 {
		return ""
	}
	isPhysicalSample := logicalChunkCount > int64(len(chunks))
	for _, chunk := range chunks {
		if chunk != nil && chunk.ProcessingGeneration != "" {
			isPhysicalSample = true
			break
		}
	}
	if !isPhysicalSample {
		return reconstructEnrichedContent(ctx, s.chunkRepo, tenantID, chunks)
	}

	sorted := append([]*types.Chunk(nil), chunks...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].ChunkIndex < sorted[j].ChunkIndex
	})
	perBlock := maxContentForWiki / len(sorted)
	if perBlock < 256 {
		perBlock = 256
	}

	var builder strings.Builder
	for _, chunk := range sorted {
		if chunk == nil || strings.TrimSpace(chunk.Content) == "" {
			continue
		}
		header := wikiSourceCoordinate(chunk)
		headerRunes := []rune(header)
		available := perBlock - len(headerRunes)
		if available <= 0 {
			continue
		}
		content := reconstructEnrichedContent(
			ctx, s.chunkRepo, tenantID, []*types.Chunk{chunk},
		)
		body := []rune(strings.TrimSpace(content))
		if len(body) > available {
			head := available * 3 / 4
			tail := available - head
			body = []rune(string(body[:head]) + "\n[…]\n" + string(body[len(body)-tail:]))
		}
		builder.WriteString(header)
		builder.WriteString(string(body))
		builder.WriteString("\n\n")
	}
	result := []rune(strings.TrimSpace(builder.String()))
	if len(result) > maxContentForWiki {
		result = result[:maxContentForWiki]
	}
	return string(result)
}

func wikiSourceCoordinate(chunk *types.Chunk) string {
	if chunk == nil || len(chunk.SourceLocator) == 0 {
		return ""
	}
	var locator map[string]any
	if err := json.Unmarshal(chunk.SourceLocator, &locator); err != nil {
		return ""
	}
	label := strings.TrimSpace(logicalLocatorHeader(locator))
	if label == "" {
		return ""
	}
	return fmt.Sprintf("【原文位置：%s】\n", label)
}

func logicalChunkLLMContent(chunk *types.Chunk, content string) string {
	if chunk == nil || strings.TrimSpace(content) == "" ||
		len(chunk.SourceLocator) == 0 {
		return content
	}
	var locator map[string]any
	if err := json.Unmarshal(chunk.SourceLocator, &locator); err != nil {
		return content
	}
	header := strings.TrimSpace(logicalLocatorHeader(locator))
	if header == "" {
		return content
	}
	return "【" + header + "】\n" + content
}
