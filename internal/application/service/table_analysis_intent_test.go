package service

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestTableAnalysisDisplayIntentPromptBlockOmitsTableIntent(t *testing.T) {
	intent := normalizeTableAnalysisDisplayIntent(&types.TableAnalysisDisplayIntent{
		ChartRequested: true,
		Confidence:     "high",
	})
	block := tableAnalysisDisplayIntentPromptBlock(intent)
	if strings.Contains(block, "table_requested") {
		t.Fatalf("display intent prompt should not include table_requested: %s", block)
	}
}
