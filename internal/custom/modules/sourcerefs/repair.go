package sourcerefs

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

var (
	plainCitationAliasRE = regexp.MustCompile(`[\(（\[【]\s*(S[1-9][0-9]*)\s*[\)）\]】]`)
	angleCitationAliasRE = regexp.MustCompile(`<\s*(S[1-9][0-9]*)\s*(?:/\s*)?>`)
	srcCitationAliasRE   = regexp.MustCompile(`(?i)<\s*src\s+id\s*=\s*["'](S[1-9][0-9]*)["']\s*/?\s*>`)
	paragraphBreakRE     = regexp.MustCompile(`\r?\n[ \t]*\r?\n+`)
	markdownPrefixRE     = regexp.MustCompile(`(?m)^\s*(?:#{1,6}\s+|[-+*>]\s+|[0-9]+[.)、]\s+)`)
)

var repairTokenStopwords = map[string]struct{}{
	"根据": {}, "依据": {}, "因此": {}, "其中": {}, "关于": {}, "相关": {}, "情况": {},
	"回答": {}, "说明": {}, "如下": {}, "可以": {}, "需要": {}, "进行": {}, "一个": {},
	"the": {}, "and": {}, "that": {}, "this": {}, "with": {}, "from": {}, "for": {},
}

// RepairAnswerCitations performs a deterministic, local-only finalization pass.
// It first normalizes plain aliases such as “（S2）” when S2 exists in the
// immutable current-turn registry. If the answer contains no citation at all,
// it may attach a handle to a paragraph only when one evidence fragment is an
// unambiguous lexical match. It never invokes a model, creates a source, or
// changes claim text.
func RepairAnswerCitations(answer string, refs []*types.SearchResult) string {
	answer = normalizeKnownCitationAliases(answer, refs)
	if strings.TrimSpace(answer) == "" {
		return answer
	}

	evidence := repairEvidence(refs)
	if len(evidence) == 0 {
		return answer
	}
	answer = relocateTrailingSourceAttributionCitation(answer, evidence)
	if canonicalSourceTagRE.MatchString(answer) {
		return attachMissingSentenceEvidence(answer, evidence, 4)
	}
	return attachUnambiguousSentenceCitations(answer, evidence, 6)
}

// relocateTrailingSourceAttributionCitation fixes a common adjacency defect:
// the model writes an evidence-backed claim, then puts the handle only on a
// following source-title paragraph. The handle is moved only when the previous
// paragraph is an unambiguous lexical match for that exact current-turn source
// and the following paragraph is visibly just a source attribution. Claim and
// source text are otherwise left unchanged.
func relocateTrailingSourceAttributionCitation(answer string, refs []citationRepairEvidence) string {
	breaks := paragraphBreakRE.FindAllStringIndex(answer, -1)
	if len(breaks) == 0 {
		return answer
	}
	paragraphs := make([]string, 0, len(breaks)+1)
	separators := make([]string, 0, len(breaks))
	start := 0
	for _, boundary := range breaks {
		paragraphs = append(paragraphs, answer[start:boundary[0]])
		separators = append(separators, answer[boundary[0]:boundary[1]])
		start = boundary[1]
	}
	paragraphs = append(paragraphs, answer[start:])

	for index := 1; index < len(paragraphs); index++ {
		current := paragraphs[index]
		if !isSourceAttributionParagraph(current) || canonicalSourceTagRE.MatchString(paragraphs[index-1]) {
			continue
		}
		ids := citationIDsInText(current)
		if len(ids) != 1 {
			continue
		}
		citationID := ""
		for id := range ids {
			citationID = id
		}
		if unambiguousEvidenceForParagraph(paragraphs[index-1], refs) != citationID {
			continue
		}
		paragraphs[index-1] = strings.TrimRight(paragraphs[index-1], " \t\r\n") + canonicalCitationTag(citationID)
		paragraphs[index] = canonicalSourceTagRE.ReplaceAllString(current, "")
	}

	var builder strings.Builder
	for index, paragraph := range paragraphs {
		builder.WriteString(paragraph)
		if index < len(separators) {
			builder.WriteString(separators[index])
		}
	}
	return builder.String()
}

func isSourceAttributionParagraph(value string) bool {
	probe := strings.TrimSpace(markdownPrefixRE.ReplaceAllString(value, ""))
	probe = canonicalSourceTagRE.ReplaceAllString(probe, "")
	probe = strings.TrimSpace(strings.Trim(probe, "*_`#> 📄📚🔗：:。.;；"))
	if probe == "" {
		return true
	}
	if strings.Contains(probe, "《") && strings.Contains(probe, "》") &&
		containsSourceAttributionMarker(probe) {
		return true
	}
	return strings.HasPrefix(probe, "来源") || strings.HasPrefix(probe, "出处") ||
		strings.HasPrefix(strings.ToLower(probe), "source:")
}

func containsSourceAttributionMarker(value string) bool {
	for _, marker := range []string{"第", "条", "款", "项", "章", "来源", "出处", "施行", "办法", "制度"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func normalizeKnownCitationAliases(answer string, refs []*types.SearchResult) string {
	known := make(map[string]struct{})
	for _, ref := range refs {
		if id := CitationID(ref); id != "" && IsSupportedCitationReference(ref) {
			known[id] = struct{}{}
		}
	}
	if len(known) == 0 {
		return answer
	}
	return transformOutsideMarkdownCode(answer, func(segment string) string {
		normalize := func(pattern *regexp.Regexp, value string) string {
			return pattern.ReplaceAllStringFunc(value, func(alias string) string {
				match := pattern.FindStringSubmatch(alias)
				if len(match) != 2 {
					return alias
				}
				if _, exists := known[match[1]]; !exists {
					return alias
				}
				return canonicalCitationTag(match[1])
			})
		}
		segment = normalize(plainCitationAliasRE, segment)
		segment = normalize(angleCitationAliasRE, segment)
		return normalize(srcCitationAliasRE, segment)
	})
}

type citationRepairEvidence struct {
	id      string
	content string
	tokens  map[string]struct{}
}

func repairEvidence(refs []*types.SearchResult) []citationRepairEvidence {
	result := make([]citationRepairEvidence, 0, len(refs))
	seen := make(map[string]struct{})
	for _, ref := range refs {
		id := CitationID(ref)
		content := strings.TrimSpace(ref.EvidenceContent)
		if id == "" || content == "" || !IsSupportedCitationReference(ref) {
			continue
		}
		key := id + "\x00" + content
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, citationRepairEvidence{
			id:      id,
			content: normalizedRepairText(content),
			tokens:  repairTokens(content),
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		return citationOrdinal(result[i].id) < citationOrdinal(result[j].id)
	})
	return result
}

func attachUnambiguousParagraphCitations(answer string, refs []citationRepairEvidence, limit int) string {
	breaks := paragraphBreakRE.FindAllStringIndex(answer, -1)
	var builder strings.Builder
	start := 0
	added := 0
	for _, boundary := range append(breaks, []int{len(answer), len(answer)}) {
		paragraph := answer[start:boundary[0]]
		if added < limit {
			if id := unambiguousEvidenceForParagraph(paragraph, refs); id != "" {
				paragraph = strings.TrimRight(paragraph, " \t\r\n") + canonicalCitationTag(id)
				added++
			}
		}
		builder.WriteString(paragraph)
		if boundary[0] < len(answer) {
			builder.WriteString(answer[boundary[0]:boundary[1]])
		}
		start = boundary[1]
	}
	return builder.String()
}

// attachUnambiguousSentenceCitations handles the zero-citation failure mode
// without assigning a source to an entire mixed paragraph. Agent answers often
// combine one evidence-derived sentence with project-specific analysis; the
// paragraph as a whole is therefore (correctly) too dissimilar to the source.
// We inspect individual claim sentences and attach only a unique strong match.
// A source is used at most once in this repair pass, keeping the result bounded
// and preventing repeated citations from being sprayed over the answer.
func attachUnambiguousSentenceCitations(answer string, refs []citationRepairEvidence, limit int) string {
	breaks := paragraphBreakRE.FindAllStringIndex(answer, -1)
	var builder strings.Builder
	start := 0
	added := 0
	used := make(map[string]struct{})
	for _, boundary := range append(breaks, []int{len(answer), len(answer)}) {
		paragraph := answer[start:boundary[0]]
		if added < limit {
			segments := splitClaimSentences(paragraph)
			var repaired strings.Builder
			for _, segment := range segments {
				if added < limit && !canonicalSourceTagRE.MatchString(segment) {
					if id := unambiguousEvidenceForParagraph(segment, refs); id != "" {
						if _, exists := used[id]; !exists {
							segment = strings.TrimRight(segment, " \t") + canonicalCitationTag(id)
							used[id] = struct{}{}
							added++
						}
					}
				}
				repaired.WriteString(segment)
			}
			paragraph = repaired.String()
		}
		builder.WriteString(paragraph)
		if boundary[0] < len(answer) {
			builder.WriteString(answer[boundary[0]:boundary[1]])
		}
		start = boundary[1]
	}
	return builder.String()
}

// attachMissingSentenceEvidence repairs a narrow but damaging citation error:
// a paragraph contains a valid citation for its later applicability sentence,
// while an earlier definition sentence in the same paragraph is supported by
// a different current-turn fragment.  The model often places only the final
// handle at paragraph end.  We add a handle only when the uncited sentence has
// one unambiguous lexical evidence match and that source is not already cited
// anywhere in the paragraph.  Existing handles and claim text are untouched.
func attachMissingSentenceEvidence(answer string, refs []citationRepairEvidence, limit int) string {
	breaks := paragraphBreakRE.FindAllStringIndex(answer, -1)
	var builder strings.Builder
	start := 0
	added := 0
	for _, boundary := range append(breaks, []int{len(answer), len(answer)}) {
		paragraph := answer[start:boundary[0]]
		if added < limit && canonicalSourceTagRE.MatchString(paragraph) {
			known := citationIDsInText(paragraph)
			segments := splitClaimSentences(paragraph)
			var repaired strings.Builder
			for _, segment := range segments {
				if added < limit && !canonicalSourceTagRE.MatchString(segment) {
					if id := unambiguousEvidenceForParagraph(segment, refs); id != "" {
						if _, alreadyCited := known[id]; !alreadyCited {
							segment = strings.TrimRight(segment, " \t") + canonicalCitationTag(id)
							known[id] = struct{}{}
							added++
						}
					}
				}
				repaired.WriteString(segment)
			}
			paragraph = repaired.String()
		}
		builder.WriteString(paragraph)
		if boundary[0] < len(answer) {
			builder.WriteString(answer[boundary[0]:boundary[1]])
		}
		start = boundary[1]
	}
	return builder.String()
}

func citationIDsInText(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, match := range canonicalSourceTagRE.FindAllStringSubmatch(value, -1) {
		if len(match) == 2 {
			result[match[1]] = struct{}{}
		}
	}
	return result
}

func splitClaimSentences(value string) []string {
	segments := make([]string, 0, 4)
	start := 0
	for index, r := range value {
		if !strings.ContainsRune("。！？!?；;", r) {
			continue
		}
		end := index + utf8.RuneLen(r)
		segments = append(segments, value[start:end])
		start = end
	}
	if start < len(value) {
		segments = append(segments, value[start:])
	}
	if len(segments) == 0 {
		return []string{value}
	}
	return segments
}

func unambiguousEvidenceForParagraph(paragraph string, refs []citationRepairEvidence) string {
	claim := strings.TrimSpace(markdownPrefixRE.ReplaceAllString(paragraph, ""))
	claim = strings.Trim(claim, "*_`#> ")
	if utf8.RuneCountInString(claim) < 12 || canonicalSourceTagRE.MatchString(claim) {
		return ""
	}
	// Short punctuation-free text is almost certainly a heading.
	if utf8.RuneCountInString(claim) <= 36 && !strings.ContainsAny(claim, "。！？；：.!?;:") {
		return ""
	}
	claimNormalized := normalizedRepairText(claim)
	claimTokens := repairTokens(claim)
	if len(claimTokens) < 3 {
		return ""
	}

	type scored struct {
		id    string
		score float64
		exact bool
	}
	scores := make([]scored, 0, len(refs))
	for _, ref := range refs {
		exact := utf8.RuneCountInString(claimNormalized) >= 12 &&
			strings.Contains(ref.content, claimNormalized)
		shared := sharedTokenCount(claimTokens, ref.tokens)
		coverage := float64(shared) / float64(len(claimTokens))
		if !exact && (shared < 3 || coverage < 0.72) {
			continue
		}
		score := coverage + float64(shared)/100.0
		if exact {
			score += 2
		}
		scores = append(scores, scored{id: ref.id, score: score, exact: exact})
	}
	if len(scores) == 0 {
		return ""
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return citationOrdinal(scores[i].id) < citationOrdinal(scores[j].id)
		}
		return scores[i].score > scores[j].score
	})
	if len(scores) > 1 && scores[0].score-scores[1].score < 0.18 {
		return ""
	}
	return scores[0].id
}

func normalizedRepairText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Join(strings.Fields(value), " ")
}

func repairTokens(value string) map[string]struct{} {
	raw := searchutil.TokenizeSimple(value)
	result := make(map[string]struct{}, len(raw))
	for token := range raw {
		if _, stop := repairTokenStopwords[token]; stop {
			continue
		}
		result[token] = struct{}{}
	}
	return result
}

func sharedTokenCount(left, right map[string]struct{}) int {
	shared := 0
	for token := range left {
		if _, exists := right[token]; exists {
			shared++
		}
	}
	return shared
}
