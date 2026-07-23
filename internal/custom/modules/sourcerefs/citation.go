package sourcerefs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	MetadataCitationID    = "citation_id"
	MetadataCitationTitle = "citation_title"
	MetadataChunkID       = "chunk_id"
)

type CitationSource struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	Title           string     `json:"title"`
	Granularity     string     `json:"granularity,omitempty"`
	KnowledgeID     string     `json:"knowledge_id,omitempty"`
	KnowledgeBaseID string     `json:"knowledge_base_id,omitempty"`
	ChunkID         string     `json:"chunk_id,omitempty"`
	ChunkIndex      int        `json:"chunk_index,omitempty"`
	StartAt         int        `json:"start_at,omitempty"`
	EndAt           int        `json:"end_at,omitempty"`
	SourceLocator   types.JSON `json:"source_locator,omitempty"`
	Slug            string     `json:"slug,omitempty"`
	URL             string     `json:"url,omitempty"`
}

type Registry struct {
	next    int
	byKey   map[string]string
	sources map[string]*CitationSource
}

func NewRegistry() *Registry {
	return &Registry{
		next:    1,
		byKey:   map[string]string{},
		sources: map[string]*CitationSource{},
	}
}

func AssignCitationIDs(refs []*types.SearchResult) []*CitationSource {
	registry := NewRegistry()
	return registry.Register(refs)
}

func (r *Registry) Register(refs []*types.SearchResult) []*CitationSource {
	if r == nil {
		return nil
	}
	out := make([]*CitationSource, 0)
	seenOut := map[string]bool{}
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		key := CitationKey(ref)
		if key == "" {
			continue
		}
		id := r.byKey[key]
		if id == "" {
			id = fmt.Sprintf("S%d", r.next)
			r.next++
			r.byKey[key] = id
			r.sources[id] = citationSourceFromRef(id, ref)
		}
		ensureMetadata(ref)
		ref.Metadata[MetadataCitationID] = id
		if src := r.sources[id]; src != nil {
			ref.Metadata[MetadataCitationTitle] = src.Title
			if src.Type != "" {
				ref.Metadata["source_type"] = src.Type
			}
			if src.URL != "" {
				ref.Metadata["url"] = src.URL
			}
			if src.Slug != "" {
				ref.Metadata["slug"] = src.Slug
			}
			if src.KnowledgeBaseID != "" {
				ref.Metadata["knowledge_base_id"] = src.KnowledgeBaseID
			}
			if src.KnowledgeID != "" {
				ref.Metadata["knowledge_id"] = src.KnowledgeID
			}
			if src.Type == SourceTypeKnowledge && src.ChunkID != "" {
				ref.Metadata[MetadataChunkID] = src.ChunkID
				ref.Metadata["chunk_index"] = strconv.Itoa(src.ChunkIndex)
				ref.Metadata["start_at"] = strconv.Itoa(src.StartAt)
				ref.Metadata["end_at"] = strconv.Itoa(src.EndAt)
				if len(src.SourceLocator) > 0 {
					ref.Metadata["source_locator"] = string(src.SourceLocator)
				}
			}
		}
		if !seenOut[id] {
			seenOut[id] = true
			out = append(out, r.sources[id])
		}
	}
	return out
}

func CitationKey(ref *types.SearchResult) string {
	if ref == nil {
		return ""
	}
	sourceType := SourceTypeFromRef(ref)
	switch sourceType {
	case SourceTypeWiki:
		slug := strings.TrimSpace(ref.Metadata["slug"])
		if slug == "" {
			slug = strings.TrimPrefix(strings.TrimSpace(ref.ID), "wiki:"+strings.TrimSpace(ref.KnowledgeBaseID)+":")
		}
		return normalizedKey(sourceType, ref.KnowledgeBaseID, slug)
	case SourceTypeWeb:
		id := normalizeURL(firstNonEmpty(ref.Metadata["url"], ref.ID))
		if id == "" {
			id = ref.KnowledgeTitle
		}
		return normalizedKey(sourceType, id)
	case SourceTypeData:
		return normalizedKey(sourceType, firstNonEmpty(ref.Metadata["source_id"], ref.ID, ref.KnowledgeTitle))
	default:
		knowledgeID := firstNonEmpty(ref.KnowledgeID, ref.Metadata["knowledge_id"])
		chunkID := knowledgeChunkID(ref)
		if chunkID != "" {
			return normalizedKey(sourceType, ref.KnowledgeBaseID, knowledgeID, chunkID)
		}
		return normalizedKey(sourceType, ref.KnowledgeBaseID, firstNonEmpty(knowledgeID, ref.KnowledgeTitle, ref.KnowledgeFilename, ref.ID))
	}
}

func SourceTypeFromRef(ref *types.SearchResult) string {
	return sourceTypeFromRef(ref)
}

func CitationID(ref *types.SearchResult) string {
	if ref == nil || ref.Metadata == nil {
		return ""
	}
	return strings.TrimSpace(ref.Metadata[MetadataCitationID])
}

func ContextCitationAttrs(ref *types.SearchResult) string {
	if ref == nil {
		return ""
	}
	id := CitationID(ref)
	if id == "" {
		return ""
	}
	title := sourceTitle(ref)
	parts := []string{
		fmt.Sprintf(`source_id="%s"`, xmlAttr(id)),
		fmt.Sprintf(`source_type="%s"`, xmlAttr(SourceTypeFromRef(ref))),
	}
	if title != "" {
		parts = append(parts, fmt.Sprintf(`source_title="%s"`, xmlAttr(title)))
	}
	if SourceTypeFromRef(ref) == SourceTypeKnowledge {
		parts = append(parts, `source_granularity="document_fragment"`)
		if chunkID := knowledgeChunkID(ref); chunkID != "" {
			parts = append(parts, fmt.Sprintf(`chunk_id="%s"`, xmlAttr(chunkID)))
			parts = append(parts, fmt.Sprintf(`chunk_index="%d"`, ref.ChunkIndex))
		}
		if locator := sourceLocatorAttribute(ref.SourceLocator); locator != "" {
			parts = append(parts, fmt.Sprintf(`source_locator="%s"`, xmlAttr(locator)))
		}
	}
	return " " + strings.Join(parts, " ")
}

func RenderCitationCatalog(refs []*types.SearchResult) string {
	sources := SourcesFromReferences(refs)
	if len(sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<citation_sources>\n")
	for _, src := range sources {
		if src == nil {
			continue
		}
		attrs := []string{
			fmt.Sprintf(`id="%s"`, xmlAttr(src.ID)),
			fmt.Sprintf(`type="%s"`, xmlAttr(src.Type)),
			fmt.Sprintf(`title="%s"`, xmlAttr(src.Title)),
		}
		switch src.Type {
		case SourceTypeKnowledge:
			attrs = append(attrs, `granularity="document_fragment"`)
			if src.KnowledgeBaseID != "" {
				attrs = append(attrs, fmt.Sprintf(`knowledge_base_id="%s"`, xmlAttr(src.KnowledgeBaseID)))
			}
			if src.KnowledgeID != "" {
				attrs = append(attrs, fmt.Sprintf(`knowledge_id="%s"`, xmlAttr(src.KnowledgeID)))
			}
			if src.ChunkID != "" {
				attrs = append(attrs, fmt.Sprintf(`chunk_id="%s"`, xmlAttr(src.ChunkID)))
				attrs = append(attrs, fmt.Sprintf(`chunk_index="%d"`, src.ChunkIndex))
			}
			if locator := sourceLocatorAttribute(src.SourceLocator); locator != "" {
				attrs = append(attrs, fmt.Sprintf(`source_locator="%s"`, xmlAttr(locator)))
			}
		case SourceTypeWiki:
			if src.Slug != "" {
				attrs = append(attrs, fmt.Sprintf(`slug="%s"`, xmlAttr(src.Slug)))
			}
		case SourceTypeWeb:
			if src.URL != "" {
				attrs = append(attrs, fmt.Sprintf(`url="%s"`, xmlAttr(src.URL)))
			}
		}
		b.WriteString("<source ")
		b.WriteString(strings.Join(attrs, " "))
		b.WriteString(" />\n")
	}
	b.WriteString("</citation_sources>")
	return b.String()
}

func SourcesFromReferences(refs []*types.SearchResult) []*CitationSource {
	sourcesByID := map[string]*CitationSource{}
	var ids []string
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		id := CitationID(ref)
		if id == "" {
			continue
		}
		if _, ok := sourcesByID[id]; ok {
			continue
		}
		src := citationSourceFromRef(id, ref)
		sourcesByID[id] = src
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		return citationOrdinal(ids[i]) < citationOrdinal(ids[j])
	})
	out := make([]*CitationSource, 0, len(ids))
	for _, id := range ids {
		out = append(out, sourcesByID[id])
	}
	return out
}

func citationSourceFromRef(id string, ref *types.SearchResult) *CitationSource {
	sourceType := SourceTypeFromRef(ref)
	src := &CitationSource{
		ID:              id,
		Type:            sourceType,
		Title:           sourceTitle(ref),
		KnowledgeID:     firstNonEmpty(ref.KnowledgeID, ref.Metadata["knowledge_id"]),
		KnowledgeBaseID: firstNonEmpty(ref.KnowledgeBaseID, ref.Metadata["knowledge_base_id"]),
	}
	switch sourceType {
	case SourceTypeWiki:
		src.Slug = firstNonEmpty(ref.Metadata["slug"], strings.TrimPrefix(ref.ID, "wiki:"+src.KnowledgeBaseID+":"))
	case SourceTypeWeb:
		src.URL = firstNonEmpty(ref.Metadata["url"], ref.ID)
	case SourceTypeData:
		src.KnowledgeID = ""
	default:
		src.Granularity = "document_fragment"
		if src.KnowledgeID == "" {
			src.KnowledgeID = strings.TrimSpace(ref.Metadata["knowledge_id"])
		}
		src.ChunkID = knowledgeChunkID(ref)
		if src.ChunkID != "" {
			src.ChunkIndex = ref.ChunkIndex
			src.StartAt = ref.StartAt
			src.EndAt = ref.EndAt
			src.SourceLocator = append(types.JSON(nil), ref.SourceLocator...)
			if len(src.SourceLocator) == 0 && ref.Metadata != nil {
				raw := []byte(strings.TrimSpace(ref.Metadata["source_locator"]))
				if json.Valid(raw) {
					src.SourceLocator = append(types.JSON(nil), raw...)
				}
			}
		}
	}
	if src.Title == "" {
		src.Title = id
	}
	return src
}

// sourceLocatorAttribute keeps the model-facing citation catalog bounded while
// CitationSource.SourceLocator retains the complete coordinate for API/UI
// consumers. Invalid JSON is never injected into the XML prompt.
func sourceLocatorAttribute(locator types.JSON) string {
	if len(locator) == 0 || !json.Valid(locator) {
		return ""
	}
	const maximumRunes = 1024
	var logical map[string]any
	if err := json.Unmarshal(locator, &logical); err != nil {
		return ""
	}
	for key := range logical {
		if key == "physical_part_index" ||
			strings.HasPrefix(key, "part_row_") ||
			strings.HasPrefix(key, "part_line_") {
			delete(logical, key)
		}
	}
	logicalLocator, err := json.Marshal(logical)
	if err != nil {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, logicalLocator); err != nil {
		return ""
	}
	value := compact.String()
	runes := []rune(value)
	if len(runes) > maximumRunes {
		return string(runes[:maximumRunes]) + "…"
	}
	return value
}

// ModelSourceLocator returns a bounded, compact original-source coordinate for
// model-facing retrieval tools. chunk_index is only a logical ordering key; a
// model must use this locator (plus record keys present in the content) when it
// cites pages, sheets, rows, lines, JSON paths, image tiles or audio ranges.
func ModelSourceLocator(locator types.JSON) string {
	return sourceLocatorAttribute(locator)
}

func sourceTitle(ref *types.SearchResult) string {
	if ref == nil {
		return ""
	}
	if ref.Metadata != nil {
		if title := firstNonEmpty(ref.Metadata[MetadataCitationTitle], ref.Metadata["source_name"], ref.Metadata["title"]); title != "" {
			return title
		}
	}
	if SourceTypeFromRef(ref) == SourceTypeWeb {
		if title := strings.TrimSpace(ref.KnowledgeTitle); title != "" {
			return title
		}
		return hostFromURL(firstNonEmpty(ref.Metadata["url"], ref.ID))
	}
	return firstNonEmpty(ref.KnowledgeTitle, ref.KnowledgeFilename, ref.KnowledgeID, ref.ID)
}

func ensureMetadata(ref *types.SearchResult) {
	if ref.Metadata == nil {
		ref.Metadata = map[string]string{}
	}
}

func knowledgeChunkID(ref *types.SearchResult) string {
	if ref == nil {
		return ""
	}
	if ref.Metadata != nil {
		if id := strings.TrimSpace(ref.Metadata[MetadataChunkID]); id != "" {
			return id
		}
	}
	id := strings.TrimSpace(ref.ID)
	if id == "" || id == strings.TrimSpace(ref.KnowledgeID) {
		return ""
	}
	return id
}

func normalizedKey(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		cleaned = append(cleaned, strings.ToLower(strings.TrimSpace(part)))
	}
	return strings.Join(cleaned, ":")
}

func normalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return strings.ToLower(raw)
	}
	parsed.Fragment = ""
	return strings.ToLower(parsed.String())
}

func xmlAttr(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		`"`, "&quot;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(value)
}

func citationOrdinal(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "S%d", &n); err != nil {
		return 0
	}
	return n
}
