package sourcerefs

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

var (
	citationLikeTagRE   = regexp.MustCompile(`(?i)</?(?:src|source|citation|doc|document|kb|wiki|web)\b[^>]*>`)
	incompleteTagTailRE = regexp.MustCompile(`(?is)</?(?:src|source|citation|doc|document|kb|wiki|web)\b[^>]*$`)
	canonicalSourceRE   = regexp.MustCompile(`^<src id="(S[1-9][0-9]*)" />$`)
	protectedCodeRE     = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|`[^`\\n]*`")
	wikiHandleRE        = regexp.MustCompile(`\[\[([^\]|\n]+)(?:\|([^\]\n]+))?\]\]`)
)

// CitationValidationReport records deterministic protocol filtering. It is
// deliberately local-only: callers must not trigger another model request.
type CitationValidationReport struct {
	CitedIDs       []string `json:"cited_ids,omitempty"`
	UnknownIDs     []string `json:"unknown_ids,omitempty"`
	ForbiddenTags  int      `json:"forbidden_tags,omitempty"`
	IncompleteTags int      `json:"incomplete_tags,omitempty"`
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
	byID := make(map[string][]*types.SearchResult)
	for _, ref := range refs {
		id := CitationID(ref)
		if id == "" {
			continue
		}
		byID[id] = append(byID[id], ref)
	}

	var report CitationValidationReport
	seenCited := make(map[string]bool)
	seenUnknown := make(map[string]bool)
	filtered := transformOutsideMarkdownCode(answer, func(segment string) string {
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
	return filtered, citedRefs, report
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
