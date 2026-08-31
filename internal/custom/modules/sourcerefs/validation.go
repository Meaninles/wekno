package sourcerefs

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
)

var (
	citationLikeTagRE    = regexp.MustCompile(`(?i)</?(?:src|source|citation|doc|document|kb|wiki|web)\b[^>]*>`)
	incompleteTagTailRE  = regexp.MustCompile(`(?is)</?(?:src|source|citation|doc|document|kb|wiki|web)\b[^>]*$`)
	canonicalSourceRE    = regexp.MustCompile(`^<src id="(S[1-9][0-9]*)" />$`)
	canonicalSourceTagRE = regexp.MustCompile(`<src id="(S[1-9][0-9]*)" />`)
	protectedCodeRE      = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|`[^`\\n]*`")
	wikiHandleRE         = regexp.MustCompile(`\[\[([^\]|\n]+)(?:\|([^\]\n]+))?\]\]`)
	markdownListMarkerRE = regexp.MustCompile(`^\s*(?:[-+*]|[0-9]+[.)、])\s+(.+?)\s*$`)
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
		segment = normalizeMarkdownListCitations(segment, byID, &report)
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

// normalizeMarkdownListCitations handles the one structural placement case
// models commonly get wrong: a handle emitted in the introductory clause just
// before a Markdown list. It moves the handle only when the immutable evidence
// snapshot contains every concise list-item label. For document fragments with
// at least four items, an obviously mismatched citation is removed instead of
// being guessed or substituted. Other prose and paraphrased/ambiguous lists
// are untouched.
func normalizeMarkdownListCitations(
	segment string,
	byID map[string][]*types.SearchResult,
	report *CitationValidationReport,
) string {
	matches := canonicalSourceTagRE.FindAllStringSubmatchIndex(segment, -1)
	for index := len(matches) - 1; index >= 0; index-- {
		match := matches[index]
		if len(match) < 4 {
			continue
		}
		id := segment[match[2]:match[3]]
		refs := byID[id]
		if len(refs) == 0 {
			continue
		}
		// Exact label coverage is safe for all three visible source kinds.
		// Only document fragments participate in mismatch removal below: Wiki
		// and web evidence have broader page shapes, so lack of a short exact
		// label must never be treated as proof that their citation is wrong.
		hasKnowledgeEvidence := false
		for _, ref := range refs {
			if ref != nil && SourceTypeFromRef(ref) == SourceTypeKnowledge {
				hasKnowledgeEvidence = true
				break
			}
		}
		_, listEnd, labels, ok := followingMarkdownList(segment, match[1])
		separatedByTransition := false
		if !ok {
			_, listEnd, labels, ok = followingMarkdownListAfterTransition(segment, match[1])
			separatedByTransition = ok
		}
		if !ok || strings.Contains(segment[match[1]:listEnd], "<src ") {
			continue
		}

		conciseLabels := make([]string, 0, len(labels))
		for _, label := range labels {
			normalized := normalizeListClaim(label)
			if length := utf8.RuneCountInString(normalized); length >= 2 && length <= 48 {
				conciseLabels = append(conciseLabels, normalized)
			}
		}
		if len(conciseLabels) < 2 {
			continue
		}
		supported := 0
		for _, label := range conciseLabels {
			if anyEvidenceContains(refs, label) {
				supported++
			}
		}
		if supported == len(conciseLabels) {
			tag := segment[match[0]:match[1]]
			if separatedByTransition {
				// The preceding citation remains correct for its own claim. Complete
				// the repeated list claim with the same already-proven handle; this
				// creates no new reference and performs no evidence guessing.
				segment = segment[:listEnd] + tag + segment[listEnd:]
				if report != nil {
					report.CompletedListCitations++
				}
			} else {
				segment = segment[:match[0]] + segment[match[1]:listEnd] + tag + segment[listEnd:]
				if report != nil {
					report.RelocatedListCitations++
				}
			}
			continue
		}
		if hasKnowledgeEvidence && len(conciseLabels) >= 4 && supported*2 < len(conciseLabels) {
			segment = segment[:match[0]] + segment[match[1]:]
			if report != nil {
				report.UnsupportedListCitations++
			}
		}
	}
	return segment
}

// followingMarkdownListAfterTransition recognizes a second conservative
// structure: a supported claim with its citation, followed by one short
// colon-ended transition line and then a repeated Markdown list. This lets the
// final list receive the same proven handle without moving the citation away
// from the preceding claim. Headings, tables, quotes, code, long prose and
// multiple transition lines are deliberately rejected.
func followingMarkdownListAfterTransition(content string, after int) (int, int, []string, bool) {
	if after < 0 || after > len(content) {
		return 0, 0, nil, false
	}
	cursor := after
	lineEnd := strings.IndexByte(content[cursor:], '\n')
	if lineEnd < 0 {
		return 0, 0, nil, false
	}
	lineEnd += cursor
	tail := strings.TrimSpace(strings.TrimSuffix(content[cursor:lineEnd], "\r"))
	if tail != "" {
		return 0, 0, nil, false
	}
	cursor = lineEnd + 1
	transitionSeen := false
	for cursor < len(content) {
		lineEnd = strings.IndexByte(content[cursor:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content)
		} else {
			lineEnd += cursor
		}
		line := strings.TrimSpace(strings.TrimSuffix(content[cursor:lineEnd], "\r"))
		if line == "" {
			if lineEnd >= len(content) {
				return 0, 0, nil, false
			}
			cursor = lineEnd + 1
			continue
		}
		if markdownListMarkerRE.MatchString(line) {
			if !transitionSeen {
				return 0, 0, nil, false
			}
			return parseMarkdownListAt(content, cursor)
		}
		if transitionSeen || utf8.RuneCountInString(line) > 80 ||
			(!strings.HasSuffix(line, ":") && !strings.HasSuffix(line, "：")) ||
			strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") ||
			strings.HasPrefix(line, "|") || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			return 0, 0, nil, false
		}
		transitionSeen = true
		if lineEnd >= len(content) {
			return 0, 0, nil, false
		}
		cursor = lineEnd + 1
	}
	return 0, 0, nil, false
}

func followingMarkdownList(content string, after int) (int, int, []string, bool) {
	if after < 0 || after > len(content) {
		return 0, 0, nil, false
	}
	cursor := after
	for cursor < len(content) && (content[cursor] == ' ' || content[cursor] == '\t') {
		cursor++
	}
	if cursor < len(content) && (content[cursor] == ':' || strings.HasPrefix(content[cursor:], "：")) {
		if content[cursor] == ':' {
			cursor++
		} else {
			cursor += len("：")
		}
	}
	for cursor < len(content) && (content[cursor] == ' ' || content[cursor] == '\t') {
		cursor++
	}
	if cursor >= len(content) || (content[cursor] != '\n' && content[cursor] != '\r') {
		return 0, 0, nil, false
	}
	cursor = consumeLineBreak(content, cursor)
	for cursor < len(content) {
		lineEnd := strings.IndexByte(content[cursor:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content)
		} else {
			lineEnd += cursor
		}
		if strings.TrimSpace(strings.TrimSuffix(content[cursor:lineEnd], "\r")) != "" {
			break
		}
		if lineEnd >= len(content) {
			return 0, 0, nil, false
		}
		cursor = lineEnd + 1
	}
	return parseMarkdownListAt(content, cursor)
}

func parseMarkdownListAt(content string, cursor int) (int, int, []string, bool) {
	listStart := cursor
	listEnd := cursor
	labels := make([]string, 0)
	for cursor < len(content) {
		lineEnd := strings.IndexByte(content[cursor:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content)
		} else {
			lineEnd += cursor
		}
		line := strings.TrimSuffix(content[cursor:lineEnd], "\r")
		marker := markdownListMarkerRE.FindStringSubmatch(line)
		if len(marker) == 2 {
			labels = append(labels, listItemLabel(marker[1]))
			listEnd = lineEnd
		} else if len(labels) > 0 && strings.TrimSpace(line) != "" &&
			(len(line) > 0 && (line[0] == ' ' || line[0] == '\t')) {
			listEnd = lineEnd
		} else {
			break
		}
		if lineEnd >= len(content) {
			break
		}
		cursor = lineEnd + 1
	}
	return listStart, listEnd, labels, len(labels) > 0 && listEnd > listStart
}

func consumeLineBreak(content string, at int) int {
	if at < len(content) && content[at] == '\r' {
		at++
	}
	if at < len(content) && content[at] == '\n' {
		at++
	}
	return at
}

func listItemLabel(value string) string {
	value = citationLikeTagRE.ReplaceAllString(value, "")
	if splitAt := strings.IndexAny(value, ":："); splitAt >= 0 {
		value = value[:splitAt]
	}
	return strings.TrimSpace(value)
}

func normalizeListClaim(value string) string {
	var out strings.Builder
	for _, current := range strings.ToLower(value) {
		if unicode.IsLetter(current) || unicode.IsNumber(current) {
			out.WriteRune(current)
		}
	}
	return out.String()
}

func anyEvidenceContains(refs []*types.SearchResult, normalizedLabel string) bool {
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		if strings.Contains(normalizeListClaim(ref.EvidenceContent), normalizedLabel) {
			return true
		}
	}
	return false
}

// collapseAdjacentDuplicateCitations keeps the first of immediately adjacent
// canonical handles that identify the same evidence. Whitespace alone does not
// make two handles semantically distinct; any real text or punctuation does.
// This is deliberately presentation-only and never changes retrieval or the
// set/order of distinct cited evidence.
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
