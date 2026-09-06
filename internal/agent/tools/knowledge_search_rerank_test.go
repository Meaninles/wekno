package tools

import (
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"testing"
)

func TestRerankThreshold_default(t *testing.T) {
	t.Parallel()
	tool := &KnowledgeSearchTool{}
	if got := tool.rerankThreshold(); got != 0.3 {
		t.Fatalf("default threshold = %v, want 0.3", got)
	}
}

func TestRerankThreshold_agentZeroDisablesAbsoluteCutoff(t *testing.T) {
	t.Parallel()
	tool := &KnowledgeSearchTool{
		agentConfig: &types.AgentConfig{RerankThreshold: 0},
		config: &config.Config{
			Conversation: &config.ConversationConfig{RerankThreshold: 0.7},
		},
	}
	if got := tool.rerankThreshold(); got != 0 {
		t.Fatalf("agent threshold = %v, want explicit zero", got)
	}
}
