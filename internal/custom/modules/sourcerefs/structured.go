package sourcerefs

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// StructuredCitation describes a location in the answer, not a source quotation.
type StructuredCitation struct {
	Text      string   `json:"text"`
	SourceIDs []string `json:"source_ids"`
	End       int      `json:"end,omitempty"`
}

// Only validated evidence from this run, never credentials or source metadata.
func StructuredEvidence(refs []*types.SearchResult) []map[string]string {
	items := []map[string]string{}
	seen := map[string]bool{}
	for _, ref := range refs {
		if !usableStructuredSource(ref) || seen[CitationID(ref)] {
			continue
		}
		seen[CitationID(ref)] = true
		items = append(items, map[string]string{"id": CitationID(ref), "title": sourceTitle(ref), "text": ref.EvidenceContent})
	}
	return items
}

// StructuredCatalog is the complete usable registry for this run. Evidence stays
// in the corresponding tool results; the catalog is never evidence by itself.
func StructuredCatalog(refs []*types.SearchResult) []map[string]string {
	items := make([]map[string]string, 0)
	seen := map[string]bool{}
	for _, ref := range refs {
		if !usableStructuredSource(ref) {
			continue
		}
		id := CitationID(ref)
		if seen[id] {
			continue
		}
		seen[id] = true
		items = append(items, map[string]string{"id": id, "title": sourceTitle(ref), "chunk_id": ref.ID})
	}
	return items
}

func usableStructuredSource(ref *types.SearchResult) bool {
	return IsSupportedCitationReference(ref) && strings.TrimSpace(ref.EvidenceContent) != "" &&
		canonicalSourceRE.MatchString(canonicalCitationTag(CitationID(ref)))
}

// RenderStructuredCitations is deterministic and never invokes a model. Invalid
// list entries/IDs are ignored individually; identical text merges at its first
// exact occurrence. Offsets are computed before any generated tag is inserted.
func RenderStructuredCitations(answer string, raw json.RawMessage, refs []*types.SearchResult) (string, []*types.SearchResult, []StructuredCitation) {
	// Only this renderer issues live tags. Leave citation syntax in code examples.
	answer = transformOutsideMarkdownCode(answer, func(s string) string {
		return incompleteTagTailRE.ReplaceAllString(citationLikeTagRE.ReplaceAllString(s, ""), "")
	})
	byID := map[string]*types.SearchResult{}
	for _, ref := range refs {
		if usableStructuredSource(ref) {
			byID[CitationID(ref)] = ref
		}
	}
	var entries []json.RawMessage
	_ = json.Unmarshal(raw, &entries)
	filtered := make([]StructuredCitation, 0)
	byText := map[string]int{}
	for _, entry := range entries {
		var item struct {
			Text string            `json:"text"`
			IDs  []json.RawMessage `json:"source_ids"`
			End  int               `json:"end"`
		}
		if json.Unmarshal(entry, &item) != nil || strings.TrimSpace(item.Text) == "" || !strings.Contains(answer, item.Text) {
			continue
		}
		end := strings.Index(answer, item.Text) + len(item.Text)
		if item.End != 0 {
			if item.End < len(item.Text) || item.End > len(answer) || answer[item.End-len(item.Text):item.End] != item.Text {
				continue
			}
			end = item.End
		}
		key := strconv.Itoa(end)
		for _, value := range item.IDs {
			var id string
			if json.Unmarshal(value, &id) != nil || byID[id] == nil {
				continue
			}
			index, exists := byText[key]
			if !exists {
				index = len(filtered)
				byText[key] = index
				filtered = append(filtered, StructuredCitation{Text: item.Text, SourceIDs: []string{}, End: item.End})
			}
			duplicate := false
			for _, previous := range filtered[index].SourceIDs {
				if previous == id {
					duplicate = true
					break
				}
			}
			if !duplicate {
				filtered[index].SourceIDs = append(filtered[index].SourceIDs, id)
			}
		}
	}
	type insertion struct {
		end      int
		citation StructuredCitation
	}
	positions := make([]insertion, 0, len(filtered))
	for _, item := range filtered {
		end := item.End
		if end == 0 {
			end = strings.Index(answer, item.Text) + len(item.Text)
		}
		positions = append(positions, insertion{end, item})
	}
	sort.SliceStable(positions, func(i, j int) bool { return positions[i].end < positions[j].end })
	used := make([]*types.SearchResult, 0)
	seen := map[string]bool{}
	for _, pos := range positions {
		for _, id := range pos.citation.SourceIDs {
			if !seen[id] {
				used = append(used, byID[id])
				seen[id] = true
			}
		}
	}
	for i := len(positions) - 1; i >= 0; i-- {
		pos := positions[i]
		tags := ""
		for _, id := range pos.citation.SourceIDs {
			tags += canonicalCitationTag(id)
		}
		answer = answer[:pos.end] + tags + answer[pos.end:]
	}
	return answer, used, filtered
}

const StructuredOutputContract = `[STRUCTURED_ANSWER_CITATIONS]
最终调用 GenerateStructuredOutput，仅提交 {"answer":"完整连贯的 Markdown 正文"}。
依据实际读取的证据回答，保留适用条件与必要限制，不凭来源标题推断内容。正文不嵌入引用标签、来源编号或引用列表。
系统随后用独立模型调用为固定正文补充引用；该阶段不改写正文，也不代替本阶段检索和核实证据。[CURRENT_RUN_SOURCES] 和工具返回的来源标识仅用于辨别证据，不复制到正文。
[/STRUCTURED_ANSWER_CITATIONS]`

func EnsureStructuredContract(prompt string) string {
	for _, name := range []string{"WEKNORA_CITATION_OUTPUT", "CITATION_USE", "STRUCTURED_ANSWER_CITATIONS"} {
		startMarker, endMarker := "["+name+"]", "[/"+name+"]"
		for {
			start := strings.Index(prompt, startMarker)
			if start < 0 {
				break
			}
			end := strings.Index(prompt[start:], endMarker)
			if end < 0 {
				break
			}
			prompt = prompt[:start] + prompt[start+end+len(endMarker):]
		}
	}
	return strings.TrimSpace(prompt) + "\n\n" + StructuredOutputContract
}
