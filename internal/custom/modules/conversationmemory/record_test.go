package conversationmemory

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestFailedAnswerIsRepresentedAsOutcomeNotAssistantProse(t *testing.T) {
	m := &types.Message{ID: "failed", Role: "assistant", Content: "这次未能完成，请稍后重试。", ErrorCode: "unknown"}
	if AssistantContent(m) != "" {
		t.Fatal("failure banner became assistant prose")
	}
	metadata := string(AssistantMetadata(m))
	if !strings.Contains(metadata, `"outcome":"failed"`) || strings.Contains(metadata, m.Content) {
		t.Fatal(metadata)
	}
}

func TestAssistantCatalogPreservesReadIdentityWithoutReplayingEvidence(t *testing.T) {
	m := &types.Message{ID: "answer", Role: "assistant", Content: "<think>private</think>Previous answer"}
	for i := 0; i < 8; i++ {
		m.KnowledgeReferences = append(m.KnowledgeReferences, &types.SearchResult{
			ID: fmt.Sprint(i), KnowledgeID: "doc", KnowledgeBaseID: "kb", KnowledgeTitle: "Source title",
			ChunkType: "text", EvidenceContent: "source body", Content: "source body",
		})
	}
	sourcerefs.AssignCitationIDs(m.KnowledgeReferences)
	got := AssistantRecord(m)
	for _, prohibited := range []string{"source body", "private", "citation_id"} {
		if strings.Contains(got, prohibited) {
			t.Fatal("historical data promoted/replayed", got)
		}
	}
	var record struct {
		SourceID string `json:"source_id"`
		Catalog  struct {
			ValidationRequired bool   `json:"validation_required"`
			Section            string `json:"section"`
			Sources            []struct {
				Pages []int  `json:"pages"`
				Title string `json:"title"`
			} `json:"sources"`
		} `json:"evidence_catalog"`
	}
	_, tail, _ := strings.Cut(got, "<conversation_record>")
	if err := json.Unmarshal([]byte(strings.TrimSuffix(tail, "</conversation_record>")), &record); err != nil {
		t.Fatal(err)
	}
	if record.SourceID != "assistant_message_answer" || !record.Catalog.ValidationRequired || record.Catalog.Section != "evidence" || len(record.Catalog.Sources) != 1 || fmt.Sprint(record.Catalog.Sources[0].Pages) != "[1 2]" {
		t.Fatal(record)
	}
	if m.KnowledgeReferences[0].EvidenceContent != "source body" || sourcerefs.CitationID(m.KnowledgeReferences[0]) != "S1" {
		t.Fatal("persisted sources changed")
	}
	m.KnowledgeReferences[0].KnowledgeTitle = strings.Repeat("long title", 10000)
	got = AssistantRecord(m)
	if EstimateTokens(got) > 512 || !strings.Contains(got, `"read_required":true`) || !strings.Contains(got, `"section":"references"`) {
		t.Fatal("catalog displaced message budget", len(got))
	}
}
