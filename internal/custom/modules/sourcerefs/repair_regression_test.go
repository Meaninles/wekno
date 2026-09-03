package sourcerefs

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// This regression deliberately uses unrelated domains and wording. It protects
// the evidence-to-sentence behavior without teaching production code any case,
// document, business-domain, or query phrase.
func TestRepairAnswerCitationsCrossDomainMissingCitationRegression(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "ops-timeout", KnowledgeID: "ops-manual", KnowledgeBaseID: "kb-ops",
			ChunkType:       string(types.ChunkTypeText),
			EvidenceContent: "The gateway closes an idle connection after 45 seconds.",
			Metadata: map[string]string{
				MetadataCitationID: "S1", MetadataChunkID: "ops-timeout", "source_type": SourceTypeKnowledge,
			},
		},
		{
			ID: "leave-window", KnowledgeID: "people-policy", KnowledgeBaseID: "kb-people",
			ChunkType:       string(types.ChunkTypeText),
			EvidenceContent: "员工应至少提前两个工作日提交普通休假申请。",
			Metadata: map[string]string{
				MetadataCitationID: "S2", MetadataChunkID: "leave-window", "source_type": SourceTypeKnowledge,
			},
		},
	}
	answer := "The gateway closes an idle connection after 45 seconds.<src id=\"S1\" />\n\n" +
		"员工应至少提前两个工作日提交普通休假申请。"

	got := RepairAnswerCitations(answer, refs)
	if strings.Count(got, `<src id="S1" />`) != 1 || strings.Count(got, `<src id="S2" />`) != 1 {
		t.Fatalf("cross-domain citation repair was incomplete or duplicated: %q", got)
	}
	if !strings.Contains(got, `普通休假申请。<src id="S2" />`) {
		t.Fatalf("missing citation was not placed beside its supported claim: %q", got)
	}
}

func TestRepairAnswerCitationsDoesNotResolveConflictingFragmentsByGuessing(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "manual-a", KnowledgeID: "device-a", KnowledgeBaseID: "kb-products",
			ChunkType: string(types.ChunkTypeText), EvidenceContent: "设备进入待机模式后保留日志七天。",
			Metadata: map[string]string{MetadataCitationID: "S1", MetadataChunkID: "manual-a", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "manual-b", KnowledgeID: "device-b", KnowledgeBaseID: "kb-products",
			ChunkType: string(types.ChunkTypeText), EvidenceContent: "设备进入待机模式后保留日志七天。",
			Metadata: map[string]string{MetadataCitationID: "S2", MetadataChunkID: "manual-b", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "设备进入待机模式后保留日志七天。"

	if got := RepairAnswerCitations(answer, refs); got != answer {
		t.Fatalf("ambiguous fragments must not cause a guessed citation: %q", got)
	}
}
