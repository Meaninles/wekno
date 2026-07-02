package types

import (
	"encoding/json"
	"testing"
)

func TestCustomAgentConfigUnmarshalKeepsExplicitZeroThresholds(t *testing.T) {
	var cfg CustomAgentConfig
	if err := json.Unmarshal([]byte(`{"vector_threshold":0,"keyword_threshold":0}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	if cfg.VectorThreshold != 0 {
		t.Fatalf("explicit vector_threshold=0 was overwritten: got %v", cfg.VectorThreshold)
	}
	if cfg.KeywordThreshold != 0 {
		t.Fatalf("explicit keyword_threshold=0 was overwritten: got %v", cfg.KeywordThreshold)
	}
	if cfg.EmbeddingTopK != 10 {
		t.Fatalf("missing embedding_top_k should keep default 10, got %d", cfg.EmbeddingTopK)
	}
}

func TestCustomAgentEnsureDefaultsDoesNotOverwriteZeroThresholds(t *testing.T) {
	agent := &CustomAgent{
		Config: CustomAgentConfig{
			VectorThreshold:  0,
			KeywordThreshold: 0,
			EmbeddingTopK:    1,
			RerankTopK:       1,
		},
	}

	agent.EnsureDefaults()

	if agent.Config.VectorThreshold != 0 {
		t.Fatalf("EnsureDefaults overwrote vector_threshold=0: got %v", agent.Config.VectorThreshold)
	}
	if agent.Config.KeywordThreshold != 0 {
		t.Fatalf("EnsureDefaults overwrote keyword_threshold=0: got %v", agent.Config.KeywordThreshold)
	}
}
