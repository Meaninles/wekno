package sourcerefs

import (
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// RegisterToolResult extracts claim-bearing evidence from a tool result,
// assigns stable run-scoped citation handles, and returns compact source
// descriptors to the caller. It never calls a model, duplicates evidence, or
// rewrites evidence content.
func RegisterToolResult(
	registry *Registry,
	toolName string,
	result *types.ToolResult,
) ([]*types.SearchResult, []*CitationSource) {
	if registry == nil || result == nil {
		return nil, nil
	}
	refs := ExtractFromToolResult(toolName, result)
	if len(refs) == 0 {
		return nil, nil
	}
	sources := registry.Register(refs)
	if len(sources) == 0 {
		return nil, nil
	}
	return refs, sources
}

// AttachToolResultSources makes handles available to runtimes whose tool
// transport exposes ToolResult.Data. Runtimes with a dedicated top-level
// source_references field should use the returned sources directly instead, so
// the same descriptors are not serialized twice.
func AttachToolResultSources(result *types.ToolResult, sources []*CitationSource) {
	if result == nil || len(sources) == 0 {
		return
	}
	if result.Data == nil {
		result.Data = make(map[string]interface{})
	}
	result.Data["source_references"] = sources
}

// AppendCitationCatalog adds only the copyable canonical handles to the
// model-visible tool output. The evidence itself remains in the original tool
// output, so this adds negligible prompt size and no duplicated source text.
func AppendCitationCatalog(output string, refs []*types.SearchResult) string {
	annotated, unresolved := AttachCitationHandlesToEvidence(output, refs)
	catalog := RenderCitationCatalog(unresolved)
	if catalog == "" || strings.Contains(output, "[AVAILABLE_CITATIONS]") {
		return annotated
	}
	if strings.TrimSpace(annotated) == "" {
		return catalog
	}
	return strings.TrimRight(annotated, "\r\n") + "\n\n" + catalog
}

// AttachCitationHandlesToEvidence places each run-scoped handle immediately
// beside the evidence block it identifies. Keeping the handle adjacent avoids
// an ambiguity that is easy for smaller/local models to make when a later tool
// result's ordinal (for example, item 6) is different from its run-scoped
// citation ID (for example, S15). The original evidence is not duplicated.
//
// References whose block cannot be identified are returned so callers can
// expose them through the compact fallback catalog instead.
func AttachCitationHandlesToEvidence(
	output string,
	refs []*types.SearchResult,
) (string, []*types.SearchResult) {
	annotated := output
	unresolved := make([]*types.SearchResult, 0)
	for _, ref := range refs {
		id := CitationID(ref)
		if ref == nil || id == "" {
			continue
		}
		marker := "citation_handle_for_this_evidence: " + canonicalCitationTag(id)
		if strings.Contains(annotated, marker) {
			continue
		}
		if SourceTypeFromRef(ref) == SourceTypeWiki {
			var inserted bool
			annotated, inserted = attachWikiCitationHandle(annotated, ref, marker)
			if !inserted {
				unresolved = append(unresolved, ref)
			}
			continue
		}

		inserted := false
		for _, anchor := range evidenceOutputAnchors(ref) {
			anchorAt := strings.Index(annotated, anchor)
			if anchorAt < 0 {
				continue
			}
			lineEndOffset := strings.IndexByte(annotated[anchorAt:], '\n')
			if lineEndOffset < 0 {
				annotated += "\n" + marker
			} else {
				insertAt := anchorAt + lineEndOffset + 1
				annotated = annotated[:insertAt] + marker + "\n" + annotated[insertAt:]
			}
			inserted = true
			break
		}
		if !inserted {
			unresolved = append(unresolved, ref)
		}
	}
	return annotated, unresolved
}

// attachWikiCitationHandle binds a handle to the wiki_page whose own metadata
// link identifies the referenced slug. Wiki outputs contain many cross-links;
// searching the whole output for the first [[slug|...]] occurrence can attach
// a source handle to a different page that merely mentions that slug.
func attachWikiCitationHandle(output string, ref *types.SearchResult, marker string) (string, bool) {
	slug := strings.TrimSpace(ref.Metadata["slug"])
	if slug == "" {
		return output, false
	}
	for _, bounds := range wikiPageRE.FindAllStringIndex(output, -1) {
		block := output[bounds[0]:bounds[1]]
		if !strings.Contains(block, "<link>[["+slug+"|") &&
			!strings.Contains(block, "<link>[["+slug+"]] </link>") &&
			!strings.Contains(block, "<link>[["+slug+"]]</link>") {
			continue
		}
		openOffset := strings.Index(block, "<wiki_page>")
		if openOffset < 0 {
			return output, false
		}
		insertAt := bounds[0] + openOffset + len("<wiki_page>")
		return output[:insertAt] + "\n" + marker + output[insertAt:], true
	}
	return output, false
}

func evidenceOutputAnchors(ref *types.SearchResult) []string {
	if ref == nil {
		return nil
	}
	switch SourceTypeFromRef(ref) {
	case SourceTypeWeb:
		if rawURL := strings.TrimSpace(ref.Metadata["url"]); rawURL != "" {
			return []string{"URL: " + rawURL, "URL:  " + rawURL}
		}
	default:
		chunkID := knowledgeChunkID(ref)
		if chunkID != "" {
			return []string{
				`chunk_id="` + chunkID + `"`,
				`faq_id="` + chunkID + `"`,
			}
		}
	}
	return nil
}
