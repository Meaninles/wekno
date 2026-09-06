package conversationmemory

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
)

var historicalThinking = regexp.MustCompile(`(?s)<think>.*?</think>`)

// EvidencePageSize is shared by the historical catalog and the validating read
// tool. Catalog entries are navigation metadata, never reusable source text.
const EvidencePageSize = 5

// AssistantRecord is the common history projection for every runtime. The
// catalog exposes which archived pages contain a source without replaying old
// tool transcripts or granting old citation IDs authority in the current run.
func AssistantRecord(m *types.Message) string {
	if m == nil {
		return ""
	}
	content := strings.TrimSpace(sourcerefs.StripCitationProtocol(historicalThinking.ReplaceAllString(m.Content, "")))
	if content == "" && len(m.KnowledgeReferences) == 0 {
		return ""
	}
	record := map[string]any{"source_id": AssistantSourceID(m.ID)}
	type source struct {
		KnowledgeBaseID string `json:"knowledge_base_id"`
		KnowledgeID     string `json:"knowledge_id,omitempty"`
		Title           string `json:"title"`
		Pages           []int  `json:"pages"`
	}
	var sources []*source
	byKey := map[string]*source{}
	for i, ref := range m.KnowledgeReferences {
		if ref == nil || !sourcerefs.IsSupportedCitationReference(ref) {
			continue
		}
		key := ref.KnowledgeBaseID + "\x00" + ref.KnowledgeID
		if ref.KnowledgeID == "" {
			key += "\x00" + ref.ID
		}
		s := byKey[key]
		if s == nil {
			s = &source{KnowledgeBaseID: ref.KnowledgeBaseID, KnowledgeID: ref.KnowledgeID, Title: ref.KnowledgeTitle}
			byKey[key] = s
			sources = append(sources, s)
		}
		page := i/EvidencePageSize + 1
		if len(s.Pages) == 0 || s.Pages[len(s.Pages)-1] != page {
			s.Pages = append(s.Pages, page)
		}
	}
	if len(sources) > 0 {
		record["evidence_catalog"] = map[string]any{
			"validation_required": true, "read_tool": "read_conversation", "section": "evidence", "sources": sources,
		}
	}
	encoded, _ := json.Marshal(record)
	// Large catalogs remain addressable without displacing user corrections
	// from the history budget. Keep entries whole rather than cutting titles.
	if EstimateTokens(string(encoded)) > 512 {
		record["evidence_catalog"] = map[string]any{
			"validation_required": true, "read_tool": "read_conversation", "section": "references",
			"source_count": len(sources), "read_required": true,
		}
		encoded, _ = json.Marshal(record)
	}
	return HistoricalAssistantOutput(content) + "\n<conversation_record>" + string(encoded) + "</conversation_record>"
}
