package chunker

import (
	"github.com/Tencent/WeKnora/internal/types"
)

// ChunkConfig is shared by ingestion, physical parts and preview.
func ChunkConfig(cc types.ChunkingConfig) SplitterConfig {
	cfg := SplitterConfig{ChunkSize: cc.ChunkSize, ChunkOverlap: cc.ChunkOverlap,
		Separators: cc.Separators, Strategy: cc.Strategy, TokenLimit: cc.TokenLimit, Languages: cc.Languages}
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = DefaultChunkSize
	}
	if cfg.ChunkOverlap < 0 {
		cfg.ChunkOverlap = 0
	}
	if len(cfg.Separators) == 0 {
		cfg.Separators = []string{"\n\n", "\n", "。"}
	}
	if cfg.Strategy == "" {
		cfg.Strategy = StrategyAuto
	}
	return cfg
}

func ParentChildConfig(cc types.ChunkingConfig, base SplitterConfig) (parent, child SplitterConfig) {
	parent, child = base, base
	parent.ChunkSize = cc.ParentChunkSize
	if parent.ChunkSize <= 0 {
		parent.ChunkSize = 4096
	}
	parent.TokenLimit = 0 // Parent context is not independently embedded.
	child.ChunkSize = cc.ChildChunkSize
	if child.ChunkSize <= 0 {
		child.ChunkSize = 384
	}
	child.ChunkOverlap = child.ChunkSize / 5
	return
}
