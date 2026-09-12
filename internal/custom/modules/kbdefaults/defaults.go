// Package kbdefaults supplies the product defaults for newly-created
// knowledge bases. It is registered once from custom/bootstrap so every
// creation caller (REST, MCP, web and mobile) uses the same behavior.
package kbdefaults

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	defaultChunkSize       = 512
	defaultChunkOverlap    = 80
	defaultParentChunkSize = 4096
	defaultChildChunkSize  = 384
	defaultASRModelName    = "qwen2.5-omni-7b"
)

type modelLister interface {
	ListModels(context.Context) ([]*types.Model, error)
}

// Service applies the default model and document-processing configuration to
// a newly-created knowledge base. Explicitly supplied values are preserved.
type Service struct {
	models modelLister
}

func NewService(models modelLister) *Service {
	return &Service{models: models}
}

// Apply is the KnowledgeBaseCreationDefaultsHook implementation.
func (s *Service) Apply(ctx context.Context, kb *types.KnowledgeBase) error {
	if s == nil || s.models == nil || kb == nil {
		return nil
	}

	models, err := s.models.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("load knowledge-base default models: %w", err)
	}

	if kb.SummaryModelID == "" {
		model := pickDefaultModel(models, types.ModelTypeKnowledgeQA)
		if model == nil {
			return fmt.Errorf("no active interactive KnowledgeQA model is configured")
		}
		kb.SummaryModelID = model.ID
	}

	if kb.NeedsEmbeddingModel() && kb.EmbeddingModelID == "" {
		model := pickDefaultModel(models, types.ModelTypeEmbedding)
		if model == nil {
			return fmt.Errorf("no active Embedding model is configured")
		}
		kb.EmbeddingModelID = model.ID
	}

	// These defaults match the document creation form. FAQ KBs do not run the
	// document parser and therefore do not receive parser/audio defaults.
	if kb.Type != types.KnowledgeBaseTypeDocument {
		return nil
	}

	if isZeroChunkingConfig(kb.ChunkingConfig) {
		kb.ChunkingConfig = defaultChunkingConfig()
	}

	// A VLLM model is optional for deployments that do not expose multimodal
	// processing. When one exists, a newly-created document KB follows the UI
	// default and enables it. A caller that explicitly supplied vlm_config is
	// never changed.
	if !kb.VLMConfigProvided && isZeroVLMConfig(kb.VLMConfig) {
		if model := pickDefaultModel(models, types.ModelTypeVLLM); model != nil {
			kb.VLMConfig = types.VLMConfig{Enabled: true, ModelID: model.ID}
		}
	}

	// Audio processing is a product-wide default. The model is intentionally
	// resolved by its stable human/model name rather than a tenant-specific ID,
	// so the same behavior works for local and production model catalogs.
	if !kb.ASRConfigProvided && isZeroASRConfig(kb.ASRConfig) {
		model := findDefaultASRModel(models)
		if model == nil {
			return fmt.Errorf("default ASR model %q is not configured", defaultASRModelName)
		}
		kb.ASRConfig = types.ASRConfig{Enabled: true, ModelID: model.ID}
	}

	return nil
}

func pickDefaultModel(models []*types.Model, modelType types.ModelType) *types.Model {
	candidates := make([]*types.Model, 0, len(models))
	for _, model := range models {
		if !isActiveModel(model, modelType) {
			continue
		}
		if modelType == types.ModelTypeKnowledgeQA &&
			model.WorkloadScope.Normalize() == types.ModelWorkloadDerivativeOnly {
			continue
		}
		candidates = append(candidates, model)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].IsDefault != candidates[j].IsDefault {
			return candidates[i].IsDefault
		}
		return candidates[i].ID < candidates[j].ID
	})
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

func findDefaultASRModel(models []*types.Model) *types.Model {
	var exact []*types.Model
	var compatible []*types.Model
	for _, model := range models {
		if !isActiveModel(model, types.ModelTypeASR) {
			continue
		}
		matchedExact := false
		matchedCompatible := false
		for _, candidate := range []string{model.Name, model.DisplayName} {
			normalized := normalizeModelName(candidate)
			if normalized == defaultASRModelName {
				matchedExact = true
			}
			if strings.Contains(normalized, defaultASRModelName) {
				matchedCompatible = true
			}
		}
		if matchedExact {
			exact = append(exact, model)
		} else if matchedCompatible {
			compatible = append(compatible, model)
		}
	}
	if model := firstPreferredModel(exact); model != nil {
		return model
	}
	return firstPreferredModel(compatible)
}

func firstPreferredModel(models []*types.Model) *types.Model {
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].IsDefault != models[j].IsDefault {
			return models[i].IsDefault
		}
		return models[i].ID < models[j].ID
	})
	if len(models) == 0 {
		return nil
	}
	return models[0]
}

func isActiveModel(model *types.Model, modelType types.ModelType) bool {
	if model == nil || model.ID == "" || model.Type != modelType {
		return false
	}
	// Empty status is accepted for legacy/in-memory model rows. Persisted
	// models use "active" and non-active states must never become defaults.
	return model.Status == "" || model.Status == types.ModelStatusActive
}

func normalizeModelName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "/models/")
	value = strings.TrimSuffix(value, ":latest")
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.Join(strings.Fields(value), "-")
	value = strings.TrimSuffix(value, "-local")
	return value
}

func isZeroChunkingConfig(config types.ChunkingConfig) bool {
	return config.ChunkSize == 0 &&
		config.ChunkOverlap == 0 &&
		len(config.Separators) == 0 &&
		len(config.ParserEngineRules) == 0 &&
		!config.EnableParentChild &&
		config.ParentChunkSize == 0 &&
		config.ChildChunkSize == 0 &&
		config.Strategy == "" &&
		config.TokenLimit == 0 &&
		len(config.Languages) == 0
}

func defaultChunkingConfig() types.ChunkingConfig {
	return types.ChunkingConfig{
		ChunkSize:         defaultChunkSize,
		ChunkOverlap:      defaultChunkOverlap,
		Separators:        []string{"\n\n", "\n", "。", "！", "？", ";", "；"},
		EnableParentChild: true,
		ParentChunkSize:   defaultParentChunkSize,
		ChildChunkSize:    defaultChildChunkSize,
		Strategy:          "auto",
	}
}

func isZeroVLMConfig(config types.VLMConfig) bool {
	return !config.Enabled && config.ModelID == "" && config.ModelName == "" &&
		config.BaseURL == "" && config.APIKey == "" && config.InterfaceType == ""
}

func isZeroASRConfig(config types.ASRConfig) bool {
	return !config.Enabled && config.ModelID == "" && config.Language == ""
}
