package knowledgeaux

import (
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/types"
	"strings"
)

// ImageAnchor keeps an extracted image searchable without attributing it to an
// unrelated paragraph when the parser supplied no inline position.
func ImageAnchor(url string, sequence int) types.ParsedChunk {
	locator, _ := json.Marshal(map[string]any{"kind": "document_image", "position_known": false, "image_url": url})
	return types.ParsedChunk{Content: "![Source image](<" + url + ">)", SourceLocator: locator, Seq: sequence, ParentIndex: -1}
}

func EnsureImageAnchors(chunks []types.ParsedChunk, urls []string) []types.ParsedChunk {
	sequence := 0
	for _, chunk := range chunks {
		sequence = max(sequence, chunk.Seq+1)
	}
	for _, url := range urls {
		if url == "" {
			continue
		}
		found := false
		for _, chunk := range chunks {
			if strings.Contains(chunk.Content, url) {
				found = true
				break
			}
		}
		if !found {
			chunks = append(chunks, ImageAnchor(url, sequence))
			sequence++
		}
	}
	return chunks
}
