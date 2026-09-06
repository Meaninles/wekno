package chatretrieval

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
)

// EvidenceText renders one exact source once. Retrieval snippets, generated
// query hints and duplicate image text remain in the diagnostic result rather
// than becoming a second model-visible copy of the source.
func EvidenceText(ref *types.SearchResult) string {
	if ref == nil {
		return ""
	}
	meta := map[string]any{"knowledge_id": ref.KnowledgeID, "knowledge_base_id": ref.KnowledgeBaseID, "title": ref.KnowledgeTitle}
	if locator := sourcerefs.ModelSourceLocator(ref.SourceLocator); locator != "" {
		if json.Valid([]byte(locator)) {
			meta["source_locator"] = json.RawMessage(locator)
		} else {
			meta["source_locator"] = locator
		}
	}
	encoded, _ := json.Marshal(meta)
	// Passage includes machine-derived image text for ranking. Only the
	// source body belongs inside the exact-fragment boundary used for quotes.
	body := *ref
	body.ImageInfo = ""
	content := Passage(&body)
	if ref.ChunkType == string(types.ChunkTypeImageOCR) || ref.ChunkType == string(types.ChunkTypeImageCaption) {
		return fmt.Sprintf("[IMAGE_OBSERVATIONS chunk_id=%q derived=true]\n%s\n%s\n[/IMAGE_OBSERVATIONS]", ref.ID, encoded, content)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "[EXACT_FRAGMENT chunk_id=%q]\n%s\n%s\n[/EXACT_FRAGMENT]", ref.ID, encoded, content)
	var images []types.ImageInfo
	if json.Unmarshal([]byte(ref.ImageInfo), &images) == nil {
		for _, im := range images {
			observation := map[string]any{"image_url": im.URL, "derived": true}
			if im.OriginalURL != "" {
				observation["original_image_url"] = im.OriginalURL
			}
			if text := strings.TrimSpace(im.OCRText); text != "" && !strings.Contains(content, text) {
				observation["ocr_text"] = text
			}
			if text := strings.TrimSpace(im.Caption); text != "" && !strings.Contains(content, text) && text != strings.TrimSpace(im.OCRText) {
				observation["description"] = text
			}
			if observation["ocr_text"] == nil && observation["description"] == nil {
				continue
			}
			data, _ := json.Marshal(observation)
			fmt.Fprintf(&out, "\n[IMAGE_OBSERVATIONS chunk_id=%q]\n%s\n[/IMAGE_OBSERVATIONS]", ref.ID, data)
		}
	}
	return out.String()
}

func FormatEvidence(refs []*types.SearchResult, kbNames map[string]string) (string, []map[string]any) {
	var out strings.Builder
	rows := make([]map[string]any, 0, len(refs))
	for i, ref := range refs {
		if i > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(EvidenceText(ref))
		row := map[string]any{"result_index": i + 1, "content": ref.Content, "knowledge_id": ref.KnowledgeID,
			"knowledge_title": ref.KnowledgeTitle, "knowledge_base_id": ref.KnowledgeBaseID,
			"knowledge_base_name": kbNames[ref.KnowledgeBaseID], "match_type": ref.MatchType,
			"matched_queries": ref.MatchedQueries, "query_scores": ref.QueryScores, "score": ref.Score,
			"chunk_id": ref.ID, "chunk_index": ref.ChunkIndex, "source_locator": ref.SourceLocator}
		if len(ref.MatchedQueries) > 0 {
			row["source_query"] = ref.MatchedQueries[0]
		}
		if ref.ChunkType == string(types.ChunkTypeFAQ) {
			row["faq_id"] = ref.ID
			row["index"] = ref.ChunkIndex
		}
		if ref.ImageInfo != "" {
			var images []types.ImageInfo
			if json.Unmarshal([]byte(ref.ImageInfo), &images) == nil {
				row["images"] = images
			}
		}
		rows = append(rows, row)
	}
	return out.String(), rows
}
