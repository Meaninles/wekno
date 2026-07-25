package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestAuxiliaryPathsFromKnowledgeReadsCoreImageFanout(t *testing.T) {
	raw, err := processownership.MarshalFanoutPlan(processownership.FanoutPlan{
		Version:              processownership.FanoutPlanVersion,
		TenantID:             10001,
		KnowledgeID:          "knowledge-a",
		KnowledgeBaseID:      "kb-a",
		ProcessingGeneration: "generation-a",
		Images: []processownership.ImageFanout{{
			ChunkID:  "chunk-a",
			ImageURL: "local://10001/knowledge-a/image-a.png",
			Index:    0,
		}},
	})
	require.NoError(t, err)

	paths, err := auxiliaryPathsFromKnowledge(&types.Knowledge{
		ProcessingFanout: types.JSON(raw),
	})
	require.NoError(t, err)
	require.Equal(t, []string{"local://10001/knowledge-a/image-a.png"}, paths)
}

func TestAuxiliaryPathsFromKnowledgeIgnoresEnrichmentRecoveryEnvelope(t *testing.T) {
	paths, err := auxiliaryPathsFromKnowledge(&types.Knowledge{
		ProcessingFanout: types.JSON(`{
			"stage":"enrichment",
			"version":3,
			"tenant_id":10001,
			"knowledge_id":"knowledge-a",
			"knowledge_base_id":"kb-a",
			"processing_generation":"generation-a",
			"text_chunk_count":181,
			"graph_batch_count":23,
			"question_batch_count":10
		}`),
	})
	require.NoError(t, err)
	require.Empty(t, paths)
}

func TestAuxiliaryPathsFromKnowledgeRejectsUnknownOrCorruptEnvelope(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown-stage":   `{"stage":"future","version":99}`,
		"corrupt-json":    `{"stage":`,
		"incomplete-core": `{"version":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := auxiliaryPathsFromKnowledge(&types.Knowledge{
				ProcessingFanout: types.JSON(raw),
			})
			require.Error(t, err)
		})
	}
}
