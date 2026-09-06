package sourcerefs

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	MetadataCitationID    = "citation_id"
	MetadataCitationTitle = "citation_title"
	MetadataChunkID       = "chunk_id"
	MetadataEvidenceHash  = "evidence_hash"
	MetadataObservedAt    = "evidence_observed_at"
	MetadataEvidenceLevel = "evidence_level"
)

type CitationSource struct {
	ID                string     `json:"id"`
	CiteExactly       string     `json:"cite_exactly"`
	Type              string     `json:"type"`
	Title             string     `json:"title"`
	Granularity       string     `json:"granularity,omitempty"`
	KnowledgeID       string     `json:"knowledge_id,omitempty"`
	KnowledgeBaseID   string     `json:"knowledge_base_id,omitempty"`
	KnowledgeBaseName string     `json:"knowledge_base_name,omitempty"`
	ChunkID           string     `json:"chunk_id,omitempty"`
	ChunkIndex        int        `json:"chunk_index,omitempty"`
	StartAt           int        `json:"start_at,omitempty"`
	EndAt             int        `json:"end_at,omitempty"`
	ResultPosition    int        `json:"result_position,omitempty"`
	SourceLocator     types.JSON `json:"source_locator,omitempty"`
	Slug              string     `json:"slug,omitempty"`
	URL               string     `json:"url,omitempty"`
	EvidenceHash      string     `json:"evidence_hash,omitempty"`
	ObservedAt        string     `json:"observed_at,omitempty"`
}

type Registry struct {
	mu      sync.RWMutex
	next    int
	byKey   map[string]string
	sources map[string]*CitationSource
	refs    map[string]*types.SearchResult
}

func NewRegistry() *Registry {
	return &Registry{
		next:    1,
		byKey:   map[string]string{},
		sources: map[string]*CitationSource{},
		refs:    map[string]*types.SearchResult{},
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
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*CitationSource, 0)
	seenOut := map[string]bool{}
	for _, ref := range refs {
		if !IsSupportedCitationReference(ref) {
			continue
		}
		ensureExactEvidenceContent(ref)
		if strings.TrimSpace(ref.EvidenceContent) == "" {
			continue
		}
		key := CitationKey(ref)
		if key == "" {
			continue
		}
		ensureMetadata(ref)
		ensureEvidenceSnapshotMetadata(ref)
		id := r.byKey[key]
		if id == "" {
			id = fmt.Sprintf("S%d", r.next)
			r.next++
			r.byKey[key] = id
		}
		selectedRef := ref
		if previousRef := r.refs[id]; previousRef != nil &&
			evidenceSnapshotPriority(ref) < evidenceSnapshotPriority(previousRef) {
			// Tool order is not a quality order. A later web_search call must not
			// replace an already fetched page with a shorter search snippet.
			selectedRef = previousRef
		}
		currentSource := citationSourceFromRef(id, selectedRef)
		if previous := r.sources[id]; previous != nil && previous.Title != "" {
			// A later deep read of the same URL/chunk refreshes the evidence
			// snapshot without downgrading a useful title discovered earlier.
			currentSource.Title = previous.Title
		}
		r.sources[id] = currentSource
		ref.Metadata[MetadataCitationID] = id
		if src := currentSource; src != nil {
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
			if src.KnowledgeBaseName != "" {
				ref.Metadata["knowledge_base_name"] = src.KnowledgeBaseName
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
			if src.EvidenceHash != "" {
				ref.Metadata[MetadataEvidenceHash] = src.EvidenceHash
			}
			if src.ObservedAt != "" {
				ref.Metadata[MetadataObservedAt] = src.ObservedAt
			}
		}
		if selectedRef == ref {
			r.refs[id] = canonicalEvidenceSnapshot(ref)
		}
		if !seenOut[id] {
			seenOut[id] = true
			out = append(out, r.sources[id])
		}
	}
	return out
}

func evidenceSnapshotPriority(ref *types.SearchResult) int {
	if ref == nil {
		return 0
	}
	if SourceTypeFromRef(ref) != SourceTypeWeb {
		return 10
	}
	switch strings.TrimSpace(ref.Metadata[MetadataEvidenceLevel]) {
	case "full_page":
		return 4
	case "fetched_content":
		return 3
	case "fetched_summary", "result_content":
		return 2
	case "search_snippet":
		return 1
	default:
		return 1
	}
}

// SnapshotReferences returns immutable, citation-id ordered evidence snapshots.
// Callers use this as the authoritative completion payload instead of relying
// on append-only reference events that may be serialized through Redis.
func (r *Registry) SnapshotReferences() []*types.SearchResult {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.refs))
	for id := range r.refs {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		return citationOrdinal(ids[i]) < citationOrdinal(ids[j])
	})
	out := make([]*types.SearchResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneSearchResult(r.refs[id]))
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

// RenderCitationCatalog gives the model one copyable, positive citation shape.
// It intentionally avoids XML <source>/<document> entries and internal chunk
// identifiers, which can prime models to emit malformed citation tags.
func RenderCitationCatalog(refs []*types.SearchResult) string {
	sources := SourcesFromReferences(refs)
	if len(sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[AVAILABLE_CITATIONS]\n")
	for _, src := range sources {
		if src == nil {
			continue
		}
		fmt.Fprintf(&b, "- evidence_id=%s | cite_exactly=%s | type=%s | title=%q",
			src.ID, src.CiteExactly, promptEvidenceType(src.Type), promptField(src.Title))
		if collection := promptField(src.KnowledgeBaseName); collection != "" {
			fmt.Fprintf(&b, " | collection=%q", collection)
		}
		b.WriteString("\n")
	}
	b.WriteString("[/AVAILABLE_CITATIONS]")
	return b.String()
}

const citationUseInstruction = `[CITATION_USE]
Cite source-supported claims with the matching provided cite_exactly handle, placed beside the supported text. Use only current registered IDs; validated reused evidence receives current IDs. Omit citations for unsupported claims and disclose material evidence gaps.
[/CITATION_USE]`

// TerminalCitationInstruction returns the single shared, positive final-output
// reminder used by every model runtime. Keeping it here prevents native RAG,
// ReAct and sidecar agents from drifting into different citation protocols.
// The reminder is generation-only: it never validates, rewrites or regenerates
// an answer.
func TerminalCitationInstruction() string {
	return citationUseInstruction
}

// HasCitableReferences reports whether refs contain at least one of the three
// user-visible citation types: document fragment, Wiki page or web page. Data
// sources remain retrieval telemetry and never activate the citation protocol.
func HasCitableReferences(refs []*types.SearchResult) bool {
	for _, ref := range refs {
		if IsSupportedCitationReference(ref) {
			return true
		}
	}
	return false
}

// PlaceTerminalCitationInstruction keeps the current-turn citation reminder at
// the end of the model-visible content, where it remains salient after long
// evidence blocks. It is idempotent and adds nothing to no-evidence turns.
func PlaceTerminalCitationInstruction(content string, refs []*types.SearchResult) string {
	if !HasCitableReferences(refs) {
		return content
	}
	withoutExisting := strings.ReplaceAll(content, citationUseInstruction, "")
	withoutExisting = strings.TrimSpace(withoutExisting)
	if withoutExisting == "" {
		return citationUseInstruction
	}
	return withoutExisting + "\n\n" + citationUseInstruction
}

// RenderEvidenceBlock associates one claim-bearing snapshot with its citation
// ID without exposing alternate XML tag shapes to the model.
func RenderEvidenceBlock(ref *types.SearchResult, content string, annotations map[string]string) string {
	if ref == nil || strings.TrimSpace(content) == "" {
		return ""
	}
	id := CitationID(ref)
	if id == "" {
		return ""
	}
	fields := []string{
		"id=" + promptField(id),
		"type=" + promptEvidenceType(SourceTypeFromRef(ref)),
	}
	if title := promptField(sourceTitle(ref)); title != "" {
		fields = append(fields, fmt.Sprintf("title=%q", title))
	}
	if collection := promptField(ref.Metadata["knowledge_base_name"]); collection != "" {
		fields = append(fields, fmt.Sprintf("collection=%q", collection))
	}
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := promptField(annotations[key])
		if key == "" || value == "" {
			continue
		}
		fields = append(fields, promptField(key)+"="+value)
	}
	return "[EVIDENCE " + strings.Join(fields, " ") + "]\n" +
		strings.TrimSpace(content) + "\n" +
		"citation_handle_for_this_evidence: " + canonicalCitationTag(id) + "\n" +
		"[/EVIDENCE]"
}

const generationContractMarker = "[WEKNORA_CITATION_OUTPUT]"

const generationContract = `[WEKNORA_CITATION_OUTPUT]
Copy a provided source handle such as <src id="S1" /> beside the claim supported by that evidence. IDs are opaque and local to this run; do not invent a handle or copy one from assistant history. The evidence itself, not its title or rank, must support the claim. Cite the minimum sufficient sources and keep citation tags outside code and link destinations.
[/WEKNORA_CITATION_OUTPUT]`

// EnsureGenerationContract applies the shared first-pass generation contract
// once. It is prompt-only and never performs validation or regeneration.
func EnsureGenerationContract(prompt string) string {
	if start := strings.Index(prompt, generationContractMarker); start >= 0 {
		endMarker := "[/WEKNORA_CITATION_OUTPUT]"
		if relativeEnd := strings.Index(prompt[start:], endMarker); relativeEnd >= 0 {
			end := start + relativeEnd + len(endMarker)
			withoutCurrent := strings.TrimSpace(prompt[:start] + prompt[end:])
			if withoutCurrent == "" {
				return generationContract
			}
			return withoutCurrent + "\n\n" + generationContract
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return generationContract
	}
	return strings.TrimSpace(prompt) + "\n\n" + generationContract
}

func canonicalCitationTag(id string) string {
	return fmt.Sprintf(`<src id="%s" />`, id)
}

func promptEvidenceType(sourceType string) string {
	if sourceType == SourceTypeKnowledge || sourceType == SourceTypeData {
		return "document_fragment"
	}
	return promptField(sourceType)
}

func promptField(value string) string {
	return strings.TrimSpace(strings.NewReplacer(
		"\r", " ",
		"\n", " ",
		"|", "/",
		"[", "(",
		"]", ")",
	).Replace(value))
}

func SourcesFromReferences(refs []*types.SearchResult) []*CitationSource {
	sourcesByID := map[string]*CitationSource{}
	var ids []string
	for _, ref := range refs {
		if !IsSupportedCitationReference(ref) {
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
		ID:                id,
		CiteExactly:       canonicalCitationTag(id),
		Type:              sourceType,
		Title:             sourceTitle(ref),
		KnowledgeID:       firstNonEmpty(ref.KnowledgeID, ref.Metadata["knowledge_id"]),
		KnowledgeBaseID:   firstNonEmpty(ref.KnowledgeBaseID, ref.Metadata["knowledge_base_id"]),
		KnowledgeBaseName: strings.TrimSpace(ref.Metadata["knowledge_base_name"]),
		EvidenceHash:      strings.TrimSpace(ref.Metadata[MetadataEvidenceHash]),
		ObservedAt:        strings.TrimSpace(ref.Metadata[MetadataObservedAt]),
	}
	if position, err := strconv.Atoi(strings.TrimSpace(ref.Metadata["tool_result_position"])); err == nil && position > 0 {
		src.ResultPosition = position
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

// sourceLocatorAttribute projects coordinates without repeating the parser's
// per-row copies of table content. API/UI retain the untouched full locator.
// The projection remains valid JSON; context budgets count its actual size.
func sourceLocatorAttribute(locator types.JSON) string {
	if len(locator) == 0 || !json.Valid(locator) {
		return ""
	}
	var logical map[string]any
	if err := json.Unmarshal(locator, &logical); err != nil {
		return ""
	}
	projectSourceCoordinates(logical)
	logicalLocator, err := json.Marshal(logical)
	if err != nil {
		return ""
	}
	return string(logicalLocator)
}

func projectSourceCoordinates(value any) {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if key == "physical_part_index" || key == "table_rows" || key == "block_id" || key == "table_id" ||
				strings.HasPrefix(key, "part_row_") || strings.HasPrefix(key, "part_line_") {
				delete(node, key)
				continue
			}
			projectSourceCoordinates(child)
		}
	case []any:
		for _, child := range node {
			projectSourceCoordinates(child)
		}
	}
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

func ensureEvidenceSnapshotMetadata(ref *types.SearchResult) {
	if ref == nil {
		return
	}
	ensureMetadata(ref)
	ensureExactEvidenceContent(ref)
	if strings.TrimSpace(ref.Metadata[MetadataEvidenceHash]) == "" {
		hash := sha256.New()
		_, _ = hash.Write([]byte(strings.TrimSpace(ref.EvidenceContent)))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(ref.SourceLocator)
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(CitationKey(ref)))
		ref.Metadata[MetadataEvidenceHash] = fmt.Sprintf("sha256:%x", hash.Sum(nil))
	}
	if strings.TrimSpace(ref.Metadata[MetadataObservedAt]) == "" {
		ref.Metadata[MetadataObservedAt] = time.Now().UTC().Format(time.RFC3339Nano)
	}
}

func ensureExactEvidenceContent(ref *types.SearchResult) {
	if ref == nil || strings.TrimSpace(ref.EvidenceContent) != "" {
		return
	}
	ref.EvidenceContent = ref.Content
}

// canonicalEvidenceSnapshot is the only shape allowed to leave the citation
// registry. Retrieval-only matched text and aggregate merge membership are
// removed, while Content mirrors EvidenceContent for non-updated API consumers.
func canonicalEvidenceSnapshot(ref *types.SearchResult) *types.SearchResult {
	clone := cloneSearchResult(ref)
	if clone == nil {
		return nil
	}
	ensureExactEvidenceContent(clone)
	clone.Content = clone.EvidenceContent
	clone.MatchedContent = ""
	clone.MatchedSourceID = ""
	clone.MatchOrigin = ""
	clone.SubChunkID = nil
	return clone
}

func cloneSearchResult(ref *types.SearchResult) *types.SearchResult {
	if ref == nil {
		return nil
	}
	clone := *ref
	if ref.Metadata != nil {
		clone.Metadata = make(map[string]string, len(ref.Metadata))
		for key, value := range ref.Metadata {
			clone.Metadata[key] = value
		}
	}
	clone.SourceLocator = append(types.JSON(nil), ref.SourceLocator...)
	clone.ChunkMetadata = append(types.JSON(nil), ref.ChunkMetadata...)
	clone.SubChunkID = append([]string(nil), ref.SubChunkID...)
	return &clone
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
	for i, part := range parts {
		value := strings.TrimSpace(part)
		if i == 0 {
			value = strings.ToLower(value)
		}
		cleaned = append(cleaned, value)
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
		return raw
	}
	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String()
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
