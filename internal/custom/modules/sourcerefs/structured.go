package sourcerefs

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// StructuredCitation describes a location in the answer, not a source quotation.
type StructuredCitation struct {
	Text      string   `json:"text"`
	SourceIDs []string `json:"source_ids"`
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
		}
		if json.Unmarshal(entry, &item) != nil || strings.TrimSpace(item.Text) == "" || !strings.Contains(answer, item.Text) {
			continue
		}
		for _, value := range item.IDs {
			var id string
			if json.Unmarshal(value, &id) != nil || byID[id] == nil {
				continue
			}
			index, exists := byText[item.Text]
			if !exists {
				index = len(filtered)
				byText[item.Text] = index
				filtered = append(filtered, StructuredCitation{Text: item.Text, SourceIDs: []string{}})
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
		positions = append(positions, insertion{strings.Index(answer, item.Text) + len(item.Text), item})
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
最终只调用 GenerateStructuredOutput，按以下 JSON 模板提交，不要另加 Markdown 代码围栏：
{"answer":"企业发展部收到立项申请后组织初审。初审未通过的，应说明理由。","citations":[{"text":"企业发展部收到立项申请后组织初审。","source_ids":["S1"]},{"text":"初审未通过的，应说明理由。","source_ids":["S2","S3"]}]}
构建步骤：先在 answer 中写完整、连贯、符合用户要求的 Markdown 正文；然后从 answer 原样复制需要引用的连续文字到 citations[].text，保留标点、空格、换行及 Markdown 格式。text 是回答正文锚点，不是来源原文。source_ids 填支持该段的本轮来源 id。
本轮后端完整可用来源列表位于每次决策的 runtime_budget 系统消息 [CURRENT_RUN_SOURCES] JSON 内，其 run_id 标识本轮。只能使用此列表的 id；chunk_id、知识条目 UUID、历史轮次 ID 均不是 source_ids。列表只有来源索引，证据原文请看相应业务工具结果或读取提供的来源文件。不得仅凭标题推断内容。工具结果中的 cite_exactly 或 citation_handle 只用于识别 S 编号，不要复制标签到 answer。
正文不要嵌入引用标签或自行编排引用数字。对资料支持的结论填写 citations；无依据的文字不配引用。无需用完全部来源，无引用时 citations 为 []。同一 text 只写一项并合并 source_ids；text 在正文出现多次时，引用只插在第一次出现的位置之后。优先选择完整句子或段落，避免代码块、行内代码和链接地址内部。后端只按精确锚点和本轮来源数据过滤，再插入引用；前端对实际使用的来源连续编号。不要复制历史无引用回答的格式。此 JSON 在本次最终回答调用中一次生成，不增加检查或修复模型调用。
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
