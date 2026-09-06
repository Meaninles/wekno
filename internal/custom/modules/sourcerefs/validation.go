package sourcerefs

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

var (
	citationLikeTagRE             = regexp.MustCompile(`(?i)</?(?:src|source|citation|doc|document|kb|wiki|web)\b[^>]*>`)
	incompleteTagTailRE           = regexp.MustCompile(`(?is)</?(?:src|source|citation|doc|document|kb|wiki|web)\b[^>]*$`)
	canonicalSourceRE             = regexp.MustCompile(`^<src id="(S[1-9][0-9]*)" />$`)
	canonicalSourceTagRE          = regexp.MustCompile(`<src id="(S[1-9][0-9]*)" />`)
	protectedCodeRE               = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|`[^`\\n]*`")
	wikiHandleRE                  = regexp.MustCompile(`\[\[([^\]|\n]+)(?:\|([^\]\n]+))?\]\]`)
	markdownListMarkerRE          = regexp.MustCompile(`^\s*(?:[-+*]|[0-9]+[.)、])\s+(.+?)\s*$`)
	markdownDestinationCitationRE = regexp.MustCompile(`(\]\([^\)\r\n]*?)[ \t]*((?:<src id="S[1-9][0-9]*" />[ \t]*)+)(\))`)
	markdownEmbeddedURLCitationRE = regexp.MustCompile(`(?i)(\]\(https?://[^\s<>()]*?)[ \t]*((?:<src id="S[1-9][0-9]*" />[ \t]*)+)[ \t]*([A-Za-z0-9][A-Za-z0-9._~:/?#@!$&'+,;=%-]*)(\))`)
	embeddedURLCitationRE         = regexp.MustCompile(`(?i)(https?://[^\s<>()]*?)[ \t]*((?:<src id="S[1-9][0-9]*" />[ \t]*)+)[ \t]*([A-Za-z0-9][A-Za-z0-9._~:/?#@!$&'+,;=%-]*)`)
	bareURLCitationRE             = regexp.MustCompile(`(?i)(https?://[^\s<>()]+)((?:<src id="S[1-9][0-9]*" />[ \t]*)+)`)
)

// CitationValidationReport records deterministic protocol filtering. It is
// deliberately local-only: callers must not trigger another model request.
type CitationValidationReport struct {
	CitedIDs                 []string `json:"cited_ids,omitempty"`
	UnknownIDs               []string `json:"unknown_ids,omitempty"`
	AvailableCount           int      `json:"available_count,omitempty"`
	EvidenceAvailableUncited bool     `json:"evidence_available_uncited,omitempty"`
	ForbiddenTags            int      `json:"forbidden_tags,omitempty"`
	IncompleteTags           int      `json:"incomplete_tags,omitempty"`
	AdjacentDuplicates       int      `json:"adjacent_duplicates,omitempty"`
	RelocatedListCitations   int      `json:"relocated_list_citations,omitempty"`
	CompletedListCitations   int      `json:"completed_list_citations,omitempty"`
	UnsupportedListCitations int      `json:"unsupported_list_citations,omitempty"`
	RelocatedURLCitations    int      `json:"relocated_url_citations,omitempty"`
}

// FilterAnswerCitations keeps only canonical, registry-backed <src id="Sx" />
// handles and returns only evidence actually cited by the answer. It never
// guesses, substitutes, or asks the model to regenerate. Markdown code spans
// and fences are preserved because citation syntax shown as code is not a
// source claim.
func FilterAnswerCitations(
	answer string,
	refs []*types.SearchResult,
) (string, []*types.SearchResult, CitationValidationReport) {
	answer = normalizeKnownCitationAliases(answer, refs)
	byID := make(map[string][]*types.SearchResult)
	for _, ref := range refs {
		if !IsSupportedCitationReference(ref) || strings.TrimSpace(ref.EvidenceContent) == "" {
			continue
		}
		id := CitationID(ref)
		if id == "" {
			continue
		}
		byID[id] = append(byID[id], ref)
	}

	report := CitationValidationReport{AvailableCount: len(byID)}
	seenCited := make(map[string]bool)
	seenUnknown := make(map[string]bool)
	filtered := transformOutsideMarkdownCode(answer, func(segment string) string {
		segment = normalizeURLCitationPlacement(segment, &report)
		segment = citationLikeTagRE.ReplaceAllStringFunc(segment, func(tag string) string {
			match := canonicalSourceRE.FindStringSubmatch(tag)
			if len(match) != 2 {
				report.ForbiddenTags++
				return ""
			}
			id := match[1]
			if len(byID[id]) == 0 {
				if !seenUnknown[id] {
					seenUnknown[id] = true
					report.UnknownIDs = append(report.UnknownIDs, id)
				}
				return ""
			}
			if !seenCited[id] {
				seenCited[id] = true
				report.CitedIDs = append(report.CitedIDs, id)
			}
			return tag
		})
		segment = incompleteTagTailRE.ReplaceAllStringFunc(segment, func(string) string {
			report.IncompleteTags++
			return ""
		})
		segment = collapseAdjacentDuplicateCitations(segment, &report)
		// Wiki's historical [[slug|title]] markup is no longer a citation
		// protocol. Preserve its readable label, but not a clickable/cited claim.
		segment = wikiHandleRE.ReplaceAllStringFunc(segment, func(value string) string {
			match := wikiHandleRE.FindStringSubmatch(value)
			if len(match) < 2 {
				return ""
			}
			report.ForbiddenTags++
			if len(match) > 2 && strings.TrimSpace(match[2]) != "" {
				return strings.TrimSpace(match[2])
			}
			return strings.TrimSpace(match[1])
		})
		return segment
	})

	citedRefs := make([]*types.SearchResult, 0)
	for _, id := range report.CitedIDs {
		citedRefs = append(citedRefs, byID[id]...)
	}
	report.EvidenceAvailableUncited = report.AvailableCount > 0 && len(report.CitedIDs) == 0 && strings.TrimSpace(filtered) != ""
	return filtered, citedRefs, report
}

// normalizeURLCitationPlacement repairs only Markdown/URL structure. A model
// can place a valid source handle before a link destination's closing ')' or
// concatenate it directly to a bare URL, causing the citation markup to become
// part of the clickable target. Move the same opaque handle outside the target;
// never change the URL, claim text, source ID, or evidence registry.
func normalizeURLCitationPlacement(segment string, report *CitationValidationReport) string {
	segment = markdownEmbeddedURLCitationRE.ReplaceAllStringFunc(segment, func(value string) string {
		match := markdownEmbeddedURLCitationRE.FindStringSubmatch(value)
		if len(match) != 5 {
			return value
		}
		if report != nil {
			report.RelocatedURLCitations += len(canonicalSourceTagRE.FindAllString(match[2], -1))
		}
		return strings.TrimRight(match[1], " \t") + match[3] + match[4] + strings.TrimSpace(match[2])
	})
	segment = embeddedURLCitationRE.ReplaceAllStringFunc(segment, func(value string) string {
		match := embeddedURLCitationRE.FindStringSubmatch(value)
		if len(match) != 4 {
			return value
		}
		if report != nil {
			report.RelocatedURLCitations += len(canonicalSourceTagRE.FindAllString(match[2], -1))
		}
		return strings.TrimRight(match[1], " \t") + match[3] + " " + strings.TrimSpace(match[2])
	})
	segment = markdownDestinationCitationRE.ReplaceAllStringFunc(segment, func(value string) string {
		match := markdownDestinationCitationRE.FindStringSubmatch(value)
		if len(match) != 4 {
			return value
		}
		if report != nil {
			report.RelocatedURLCitations += len(canonicalSourceTagRE.FindAllString(match[2], -1))
		}
		return strings.TrimRight(match[1], " \t") + match[3] + strings.TrimSpace(match[2])
	})
	segment = bareURLCitationRE.ReplaceAllStringFunc(segment, func(value string) string {
		match := bareURLCitationRE.FindStringSubmatch(value)
		if len(match) != 3 {
			return value
		}
		if report != nil {
			report.RelocatedURLCitations += len(canonicalSourceTagRE.FindAllString(match[2], -1))
		}
		return match[1] + " " + strings.TrimSpace(match[2])
	})
	return segment
}

// normalizeMarkdownListCitations handles the one structural placement case
// models commonly get wrong: a handle emitted in the introductory clause just
// before a Markdown list. It moves the handle only when the immutable evidence
// snapshot contains every concise list-item label. For document fragments with
// at least four items, an obviously mismatched citation is removed instead of
// being guessed or substituted. Other prose and paraphrased/ambiguous lists
// are untouched.
func collapseAdjacentDuplicateCitations(segment string, report *CitationValidationReport) string {
	matches := canonicalSourceTagRE.FindAllStringSubmatchIndex(segment, -1)
	if len(matches) < 2 {
		return segment
	}
	var out strings.Builder
	out.Grow(len(segment))
	start := 0
	previousID := ""
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		between := segment[start:match[0]]
		id := segment[match[2]:match[3]]
		if id == previousID && strings.TrimSpace(between) == "" {
			if report != nil {
				report.AdjacentDuplicates++
			}
			start = match[1]
			continue
		}
		out.WriteString(between)
		out.WriteString(segment[match[0]:match[1]])
		start = match[1]
		previousID = id
	}
	out.WriteString(segment[start:])
	return out.String()
}

// StripCitationProtocol removes stale handles before a historical assistant
// answer is supplied to another model turn. Source IDs are request-local and
// must only be re-issued from evidence retrieved in the current turn.
func StripCitationProtocol(content string) string {
	return transformOutsideMarkdownCode(content, func(segment string) string {
		segment = citationLikeTagRE.ReplaceAllString(segment, "")
		segment = incompleteTagTailRE.ReplaceAllString(segment, "")
		return wikiHandleRE.ReplaceAllStringFunc(segment, func(value string) string {
			match := wikiHandleRE.FindStringSubmatch(value)
			if len(match) > 2 && strings.TrimSpace(match[2]) != "" {
				return strings.TrimSpace(match[2])
			}
			if len(match) > 1 {
				return strings.TrimSpace(match[1])
			}
			return ""
		})
	})
}

func transformOutsideMarkdownCode(content string, transform func(string) string) string {
	if content == "" || transform == nil {
		return content
	}
	indices := protectedCodeRE.FindAllStringIndex(content, -1)
	if len(indices) == 0 {
		return transform(content)
	}
	var out strings.Builder
	out.Grow(len(content))
	start := 0
	for _, index := range indices {
		out.WriteString(transform(content[start:index[0]]))
		out.WriteString(content[index[0]:index[1]])
		start = index[1]
	}
	out.WriteString(transform(content[start:]))
	return out.String()
}

// DecodeSearchResults preserves every SearchResult field when event data has
// crossed a JSON/Redis boundary and arrived as []interface{} or map values.
func DecodeSearchResults(value interface{}) []*types.SearchResult {
	if value == nil {
		return nil
	}
	if typed, ok := value.([]*types.SearchResult); ok {
		out := make([]*types.SearchResult, 0, len(typed))
		for _, ref := range typed {
			if ref != nil {
				out = append(out, cloneSearchResult(ref))
			}
		}
		return out
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var refs []*types.SearchResult
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil
	}
	out := refs[:0]
	for _, ref := range refs {
		if ref != nil {
			out = append(out, ref)
		}
	}
	return out
}
