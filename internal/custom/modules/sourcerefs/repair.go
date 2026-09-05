package sourcerefs

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

var (
	plainCitationAliasRE            = regexp.MustCompile(`[\(（\[【]\s*(S[1-9][0-9]*)\s*[\)）\]】]`)
	parenthesizedSrcCitationAliasRE = regexp.MustCompile(`(?i)[\(（\[【]\s*src\s+id\s*=\s*["']?(S[1-9][0-9]*)["']?\s*[\)）\]】]`)
	angleCitationAliasRE            = regexp.MustCompile(`<\s*(S[1-9][0-9]*)\s*(?:/\s*)?>`)
	srcCitationAliasRE              = regexp.MustCompile(`(?i)<\s*src\s+id\s*=\s*["'](S[1-9][0-9]*)["']\s*/?\s*>`)
	paragraphBreakRE                = regexp.MustCompile(`\r?\n[ \t]*\r?\n+`)
	markdownPrefixRE                = regexp.MustCompile(`(?m)^\s*(?:#{1,6}\s+|[-+*>]\s+|[0-9]+[.)、]\s+)`)
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
	answer = relocateQuestionCitationToSupportedAnswer(answer, evidence)
	if canonicalSourceTagRE.MatchString(answer) {
		answer = attachMissingSentenceEvidence(answer, evidence, 4)
		return attachUnambiguousUncitedParagraphCitations(answer, evidence, 4)
	}
	return attachUnambiguousSentenceCitations(answer, evidence, 6)
}

// attachUnambiguousUncitedParagraphCitations covers a mixed answer in which
// one paragraph is cited correctly while another independent claim paragraph
// omits its handle. Existing cited paragraphs are left untouched (so a
// paragraph-end citation is not duplicated); only a wholly uncited paragraph
// with one unambiguous lexical evidence match may receive a handle.
func attachUnambiguousUncitedParagraphCitations(
	answer string, refs []citationRepairEvidence, limit int,
) string {
	breaks := paragraphBreakRE.FindAllStringIndex(answer, -1)
	var builder strings.Builder
	start := 0
	added := 0
	for _, boundary := range append(breaks, []int{len(answer), len(answer)}) {
		paragraph := answer[start:boundary[0]]
		if added < limit && !canonicalSourceTagRE.MatchString(paragraph) &&
			!isSourceAttributionParagraph(paragraph) {
			before := len(canonicalSourceTagRE.FindAllString(paragraph, -1))
			paragraph = attachUnambiguousSentenceCitations(paragraph, refs, limit-added)
			after := len(canonicalSourceTagRE.FindAllString(paragraph, -1))
			added += after - before
		}
		builder.WriteString(paragraph)
		if boundary[0] < len(answer) {
			builder.WriteString(answer[boundary[0]:boundary[1]])
		}
		start = boundary[1]
	}
	return builder.String()
}

// relocateQuestionCitationToSupportedAnswer handles a citation placed after a
// restated question while the immediately following answer sentence contains
// the actual evidence-backed claim. It moves (rather than copies) the handle,
// and only when that sentence has a unique lexical match to the same immutable
// current-turn evidence fragment.
func relocateQuestionCitationToSupportedAnswer(answer string, refs []citationRepairEvidence) string {
	paragraphs := paragraphBreakRE.Split(answer, -1)
	changed := false
	for index := 0; index+1 < len(paragraphs); index++ {
		question := paragraphs[index]
		ids := citationIDsInText(question)
		if len(ids) != 1 {
			continue
		}
		withoutCitation := canonicalSourceTagRE.ReplaceAllString(question, "")
		if !strings.ContainsAny(withoutCitation, "？?") {
			continue
		}
		id := ""
		for candidate := range ids {
			id = candidate
		}
		repaired, ok := attachCitationToUniqueSupportedSentence(paragraphs[index+1], id, refs)
		if !ok {
			continue
		}
		paragraphs[index] = strings.TrimRight(withoutCitation, " \t")
		paragraphs[index+1] = repaired
		changed = true
	}
	if !changed {
		return answer
	}
	return strings.Join(paragraphs, "\n\n")
}

func attachCitationToUniqueSupportedSentence(
	paragraph, citationID string,
	refs []citationRepairEvidence,
) (string, bool) {
	if strings.TrimSpace(paragraph) == "" || citationID == "" || canonicalSourceTagRE.MatchString(paragraph) {
		return paragraph, false
	}
	segments := splitClaimSentences(paragraph)
	matched := -1
	for index, segment := range segments {
		if canonicalSourceTagRE.MatchString(segment) || unambiguousEvidenceForParagraph(segment, refs) != citationID {
			continue
		}
		if matched >= 0 {
			return paragraph, false
		}
		matched = index
	}
	if matched < 0 {
		return paragraph, false
	}
	segments[matched] = strings.TrimRight(segments[matched], " \t") + canonicalCitationTag(citationID)
	return strings.Join(segments, ""), true
}

// RepairNamedTopicCitationBindings fixes a narrow, high-confidence comparison
// failure without asking the model to regenerate. When one named option's own
// paragraph has exactly one handle, that handle's evidence does not name the
// option, and a different current-turn fragment is the unique strong lexical
// match that does name it, only the opaque handle is replaced. Claim text and
// the immutable evidence registry are never changed. Ambiguous cases remain
// untouched so ordinary answers cannot acquire guessed citations.
func RepairNamedTopicCitationBindings(
	answer string,
	topics []string,
	refs []*types.SearchResult,
) string {
	if strings.TrimSpace(answer) == "" || len(topics) < 2 {
		return answer
	}
	evidence := repairEvidence(refs)
	if len(evidence) == 0 {
		return answer
	}
	answer = relocateUnscopedNamedTopicCitation(answer, topics, evidence)

	breaks := paragraphBreakRE.FindAllStringIndex(answer, -1)
	var builder strings.Builder
	start := 0
	for _, boundary := range append(breaks, []int{len(answer), len(answer)}) {
		paragraph := answer[start:boundary[0]]
		matchedTopics := repairTopicsForParagraph(paragraph, topics)
		citationIDs := citationIDsInText(paragraph)
		rebuiltCondition := false
		if len(matchedTopics) == 1 && len(citationIDs) > 1 &&
			paragraphClaimsApplicabilityConditions(paragraph) {
			topic := matchedTopics[0]
			allCitationsDirect := true
			for id := range citationIDs {
				if !citationEvidenceSupportsNamedTopicClaim(id, topic, paragraph, evidence) {
					allCitationsDirect = false
					break
				}
			}
			if !allCitationsDirect {
				if replacementID := uniqueNamedTopicConditionEvidence(topic, evidence); replacementID != "" {
					if excerpt := namedTopicConditionExcerpt(replacementID, topic, evidence); excerpt != "" {
						paragraph = renderGroundedConditionParagraph(paragraph, topic, excerpt, replacementID)
						rebuiltCondition = true
					}
				}
			}
		}
		if !rebuiltCondition && len(matchedTopics) == 1 && len(citationIDs) == 1 {
			currentID := ""
			for id := range citationIDs {
				currentID = id
			}
			topic := matchedTopics[0]
			if !citationEvidenceSupportsNamedTopicClaim(currentID, topic, paragraph, evidence) {
				if replacementID := strongestNamedTopicEvidence(paragraph, topic, evidence); replacementID != "" && replacementID != currentID {
					if paragraphClaimsApplicabilityConditions(paragraph) {
						if excerpt := namedTopicConditionExcerpt(replacementID, topic, evidence); excerpt != "" {
							paragraph = renderGroundedConditionParagraph(paragraph, topic, excerpt, replacementID)
						} else {
							paragraph = replaceCitationID(paragraph, currentID, replacementID)
						}
					} else {
						paragraph = replaceCitationID(paragraph, currentID, replacementID)
					}
				}
			}
		} else if len(matchedTopics) == 1 && len(citationIDs) == 0 &&
			paragraphClaimsApplicabilityConditions(paragraph) && paragraphReportsMissingEvidence(paragraph) {
			topic := matchedTopics[0]
			if replacementID := strongestNamedTopicEvidence(paragraph, topic, evidence); replacementID != "" {
				if excerpt := namedTopicConditionExcerpt(replacementID, topic, evidence); excerpt != "" {
					paragraph = renderGroundedConditionParagraph(paragraph, topic, excerpt, replacementID)
				}
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

// relocateUnscopedNamedTopicCitation repairs a bounded definition-layout
// failure without increasing the citation count. A model may spend a shared
// evidence handle on a source-only preface (for example, "according to article
// 33") while one explicitly requested option paragraph remains uncited. When
// that same immutable fragment is the unique direct definition evidence for
// the missing option, move the preface handle to that option paragraph.
func relocateUnscopedNamedTopicCitation(
	answer string,
	topics []string,
	refs []citationRepairEvidence,
) string {
	breaks := paragraphBreakRE.FindAllStringIndex(answer, -1)
	paragraphs := make([]string, 0, len(breaks)+1)
	separators := make([]string, 0, len(breaks))
	start := 0
	for _, boundary := range breaks {
		paragraphs = append(paragraphs, answer[start:boundary[0]])
		separators = append(separators, answer[boundary[0]:boundary[1]])
		start = boundary[1]
	}
	paragraphs = append(paragraphs, answer[start:])

	for targetIndex, paragraph := range paragraphs {
		matched := repairTopicsForParagraph(paragraph, topics)
		if len(matched) != 1 || canonicalSourceTagRE.MatchString(paragraph) {
			continue
		}
		citationID := uniqueNamedDefinitionEvidenceForParagraph(matched[0], paragraph, refs)
		if citationID == "" {
			continue
		}
		for donorIndex, donor := range paragraphs {
			if donorIndex == targetIndex || !isSourceAttributionParagraph(donor) ||
				len(repairTopicsForParagraph(donor, topics)) != 0 {
				continue
			}
			if _, present := citationIDsInText(donor)[citationID]; !present {
				continue
			}
			paragraphs[donorIndex] = removeCitationID(donor, citationID)
			paragraphs[targetIndex] = strings.TrimRight(paragraph, " \t\r\n") +
				canonicalCitationTag(citationID)
			return joinRepairParagraphs(paragraphs, separators)
		}
	}
	return answer
}

func uniqueNamedDefinitionEvidenceForParagraph(
	topic string,
	paragraph string,
	refs []citationRepairEvidence,
) string {
	topicProbe := normalizedNamedTopicText(topic)
	claimTokens := repairTokens(canonicalSourceTagRE.ReplaceAllString(paragraph, ""))
	match := ""
	for _, ref := range refs {
		contentProbe := normalizedNamedTopicText(ref.content)
		if !strings.Contains(contentProbe, topicProbe) ||
			!namedDefinitionEvidence(ref.content, topic) ||
			sharedTokenCount(claimTokens, ref.tokens) < 3 {
			continue
		}
		if match != "" && match != ref.id {
			return ""
		}
		match = ref.id
	}
	return match
}

func namedDefinitionEvidence(content, topic string) bool {
	topicProbe := normalizedNamedTopicText(topic)
	for _, sentence := range splitClaimSentences(content) {
		probe := normalizedNamedTopicText(sentence)
		topicAt := strings.Index(probe, topicProbe)
		if topicAt < 0 {
			continue
		}
		tail := probe[topicAt+len(topicProbe):]
		if marker := strings.Index(tail, "是指"); marker >= 0 && marker <= 36 {
			return true
		}
	}
	return false
}

func removeCitationID(value, citationID string) string {
	return canonicalSourceTagRE.ReplaceAllStringFunc(value, func(tag string) string {
		match := canonicalSourceTagRE.FindStringSubmatch(tag)
		if len(match) == 2 && match[1] == citationID {
			return ""
		}
		return tag
	})
}

func joinRepairParagraphs(paragraphs, separators []string) string {
	var builder strings.Builder
	for index, paragraph := range paragraphs {
		builder.WriteString(paragraph)
		if index < len(separators) {
			builder.WriteString(separators[index])
		}
	}
	return builder.String()
}

// repairTopicsForParagraph prefers the option named at the start of a
// paragraph. A model may append a cross-option comparison to an otherwise
// single-option paragraph; treating every later name as paragraph ownership
// prevented the actual option's bad citation from being repaired.
func repairTopicsForParagraph(paragraph string, topics []string) []string {
	lead := ""
	for _, line := range strings.Split(strings.ReplaceAll(paragraph, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lead = strings.TrimSpace(markdownPrefixRE.ReplaceAllString(line, ""))
		lead = strings.TrimSpace(strings.Trim(lead, "*_`#> "))
		break
	}
	normalizedLead := normalizedNamedTopicText(lead)
	leading := make([]string, 0, 2)
	for _, topic := range topics {
		normalizedTopic := normalizedNamedTopicText(topic)
		if normalizedTopic != "" && strings.HasPrefix(normalizedLead, normalizedTopic) {
			leading = append(leading, topic)
		}
	}
	if len(leading) == 1 {
		return leading
	}
	return namedTopicsInParagraph(paragraph, topics)
}

// EnsureNamedTopicDefinitions restores a definition explicitly requested for
// every named comparison option when the generated option paragraph omitted it
// or the model omitted that option's paragraph entirely.
// The inserted sentence is copied verbatim from a unique current-turn evidence
// fragment and carries that fragment's immutable citation handle. No domain
// wording, inferred fact, or model call is introduced.
func EnsureNamedTopicDefinitions(
	answer string,
	topics []string,
	refs []*types.SearchResult,
) string {
	value := strings.TrimSpace(answer)
	if value == "" || len(topics) < 2 {
		return value
	}
	evidence := repairEvidence(refs)
	if len(evidence) == 0 {
		return value
	}

	type definition struct {
		text string
		id   string
	}
	missing := make(map[string]definition, len(topics))
	for _, topic := range topics {
		if answerHasNamedDefinition(value, topic) {
			continue
		}
		text, id := bestNamedDefinitionEvidence(topic, evidence)
		if text != "" && id != "" {
			missing[topic] = definition{text: text, id: id}
		}
	}
	if len(missing) == 0 {
		return value
	}

	paragraphs := paragraphBreakRE.Split(value, -1)
	changed := false
	for index, paragraph := range paragraphs {
		matched := repairTopicsForParagraph(paragraph, topics)
		if len(matched) != 1 {
			continue
		}
		item, exists := missing[matched[0]]
		if !exists {
			continue
		}
		paragraphs[index] = item.text + canonicalCitationTag(item.id) + " " + strings.TrimSpace(paragraph)
		delete(missing, matched[0])
		changed = true
	}
	// A model can retrieve every requested option and still stop after writing
	// only the first one or two paragraphs.  There is no paragraph into which
	// the loop above can insert the missing definition, so append one compact,
	// extractive paragraph per remaining explicit topic in the user's order.
	// Each sentence still comes verbatim from current-turn evidence and carries
	// its immutable handle; topics without such evidence remain untouched.
	for _, topic := range topics {
		item, exists := missing[topic]
		if !exists {
			continue
		}
		paragraphs = append(paragraphs, item.text+canonicalCitationTag(item.id))
		delete(missing, topic)
		changed = true
	}
	if !changed {
		return value
	}
	return strings.TrimSpace(strings.Join(paragraphs, "\n\n"))
}

func answerHasNamedDefinition(answer, topic string) bool {
	for _, paragraph := range paragraphBreakRE.Split(answer, -1) {
		matched := repairTopicsForParagraph(paragraph, []string{topic})
		if len(matched) != 1 {
			continue
		}
		probe := normalizedNamedTopicText(paragraph)
		topicProbe := normalizedNamedTopicText(topic)
		topicAt := strings.Index(probe, topicProbe)
		if topicAt < 0 {
			continue
		}
		tail := probe[topicAt+len(topicProbe):]
		if marker := definitionMarkerIndex(tail); marker >= 0 && marker <= 36 {
			return true
		}
	}
	return false
}

func bestNamedDefinitionEvidence(topic string, refs []citationRepairEvidence) (string, string) {
	topicProbe := normalizedNamedTopicText(topic)
	bestText, bestID := "", ""
	bestLength := int(^uint(0) >> 1)
	seenText := make(map[string]bool)
	for _, ref := range refs {
		for _, sentence := range splitClaimSentences(ref.content) {
			text := strings.TrimSpace(strings.Join(strings.Fields(sentence), " "))
			probe := normalizedNamedTopicText(text)
			topicAt := strings.Index(probe, topicProbe)
			if topicAt < 0 {
				continue
			}
			tail := probe[topicAt+len(topicProbe):]
			markerAt := definitionMarkerIndex(tail)
			if markerAt < 0 || markerAt > 36 {
				continue
			}
			length := utf8.RuneCountInString(text)
			if length < 8 || length > 260 || seenText[probe] {
				continue
			}
			seenText[probe] = true
			if length < bestLength || (length == bestLength && citationOrdinal(ref.id) < citationOrdinal(bestID)) {
				bestText, bestID, bestLength = text, ref.id, length
			}
		}
	}
	return bestText, bestID
}

// definitionMarkerIndex recognizes the grammatical definition relation after
// a named topic without enumerating any business noun. Prefer the complete
// copular form and fall back to the generic Chinese definition verb.
func definitionMarkerIndex(value string) int {
	if index := strings.Index(value, "是指"); index >= 0 {
		return index
	}
	return strings.Index(value, "指")
}

// RecoverOffTopicNarrowEvidenceAnswer replaces a stale answer to an explicitly
// narrow evidence turn with short extractive answers from the current-turn
// evidence registry. Multi-question turns still fail open when any requested
// topic is already present. A single-question turn is also recovered when the
// answer starts correctly but then drifts back into a long historical topic;
// the replacement is allowed only when current evidence directly matches it.
func RecoverOffTopicNarrowEvidenceAnswer(
	answer string,
	topics []string,
	refs []*types.SearchResult,
	originalQuery ...string,
) string {
	value := strings.TrimSpace(answer)
	if value == "" || len(topics) == 0 {
		return value
	}
	query := ""
	if len(originalQuery) > 0 {
		query = originalQuery[0]
	}
	quantitativeIncomplete := false
	if query != "" {
		for _, topic := range topics {
			if unit := narrowQuantitativeQuestionUnit(query, topic); unit != "" &&
				!answerContainsNarrowQuantitativeClaim(value, topic, unit) {
				quantitativeIncomplete = true
				break
			}
		}
	}
	aligned := false
	for _, topic := range topics {
		if evidenceTextMatchesTopic(value, topic) {
			aligned = true
			break
		}
	}
	if aligned && !quantitativeIncomplete && (len(topics) != 1 || utf8.RuneCountInString(value) <= 500) {
		return value
	}
	evidence := repairEvidence(refs)
	if len(evidence) == 0 {
		return value
	}
	lines := make([]string, 0, len(topics))
	for _, topic := range topics {
		excerpt, id := bestNarrowTopicEvidenceForQuestion(topic, query, evidence)
		if excerpt == "" || id == "" {
			return value
		}
		lines = append(lines, excerpt+canonicalCitationTag(id))
	}
	result := strings.TrimSpace(strings.Join(lines, "\n\n"))
	if utf8.RuneCountInString(result) > 500 {
		return value
	}
	return result
}

type narrowTopicAnchor struct {
	prefix string
	suffix string
}

func narrowTopicAnchors(topic string) []narrowTopicAnchor {
	value := strings.TrimSpace(topic)
	for _, marker := range []string{
		"如果", "那么", "以及", "并且", "涉及", "并影响", "影响", "至少", "达到",
		"是否", "能否", "可否", "哪些", "哪个", "哪位", "多少", "如何", "怎么", "由",
	} {
		value = strings.ReplaceAll(value, marker, "|")
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '|' || r == '，' || r == ',' || r == '；' || r == ';' || r == '、' || unicode.IsSpace(r)
	})
	anchors := make([]narrowTopicAnchor, 0, len(parts))
	for _, part := range parts {
		probe := normalizedNamedTopicText(part)
		runes := []rune(probe)
		if len(runes) < 2 {
			continue
		}
		anchor := narrowTopicAnchor{prefix: string(runes), suffix: string(runes)}
		if len(runes) >= 4 {
			anchor.prefix = string(runes[:2])
			anchor.suffix = string(runes[len(runes)-2:])
		}
		anchors = append(anchors, anchor)
	}
	return anchors
}

func evidenceTextMatchesTopic(text, topic string) bool {
	probe := normalizedNamedTopicText(text)
	anchors := narrowTopicAnchors(topic)
	if len(anchors) == 0 {
		return false
	}
	matched := make([]bool, len(anchors))
	for index, anchor := range anchors {
		matched[index] = strings.Contains(probe, anchor.prefix) && strings.Contains(probe, anchor.suffix)
	}
	for _, ok := range matched {
		if !ok {
			// Match the retrieval fallback: with at least three independent
			// anchors, only the first framing noun may differ.  Every condition
			// and consequence anchor must still be present in the same excerpt.
			if len(matched) >= 3 && !matched[0] {
				for _, remainderOK := range matched[1:] {
					if !remainderOK {
						return false
					}
				}
				return true
			}
			return false
		}
	}
	return true
}

func bestNarrowTopicEvidence(topic string, refs []citationRepairEvidence) (string, string) {
	return bestNarrowTopicEvidenceForQuestion(topic, "", refs)
}

func bestNarrowTopicEvidenceForQuestion(
	topic string,
	query string,
	refs []citationRepairEvidence,
) (string, string) {
	bestText, bestID := "", ""
	bestLength := int(^uint(0) >> 1)
	seenText := make(map[string]bool)
	unit := narrowQuantitativeQuestionUnit(query, topic)
	for _, ref := range refs {
		segments := splitClaimSentences(ref.content)
		candidates := append([]string{}, segments...)
		for index := 0; index+1 < len(segments); index++ {
			candidates = append(candidates, segments[index]+segments[index+1])
		}
		for _, candidate := range candidates {
			text := strings.TrimSpace(strings.Join(strings.Fields(candidate), " "))
			if !evidenceTextMatchesTopic(text, topic) ||
				(unit != "" && !containsNumberWithUnit(text, unit)) {
				continue
			}
			probe := normalizedNamedTopicText(text)
			length := utf8.RuneCountInString(text)
			if length < 4 || length > 240 || seenText[probe] {
				continue
			}
			seenText[probe] = true
			if length < bestLength || (length == bestLength && citationOrdinal(ref.id) < citationOrdinal(bestID)) {
				bestText, bestID, bestLength = text, ref.id, length
			}
		}
	}
	return bestText, bestID
}

func narrowQuantitativeQuestionUnit(query, topic string) string {
	value := query
	if index := strings.Index(value, "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"); index >= 0 {
		value = value[:index]
	}
	for _, clause := range regexp.MustCompile(`[？?\n]+`).Split(value, -1) {
		topicAt := strings.Index(clause, topic)
		if topicAt < 0 {
			continue
		}
		tail := clause[topicAt+len(topic):]
		questionAt := strings.Index(tail, "多少")
		if questionAt < 0 {
			continue
		}
		unitTail := strings.TrimSpace(tail[questionAt+len("多少"):])
		unitRunes := make([]rune, 0, 8)
		for _, r := range unitTail {
			if unicode.IsSpace(r) || strings.ContainsRune("，,；;。.!！?？：:、（）()[]【】", r) || !unicode.IsLetter(r) {
				break
			}
			unitRunes = append(unitRunes, r)
			if len(unitRunes) == 8 {
				break
			}
		}
		return strings.TrimPrefix(strings.TrimSpace(string(unitRunes)), "个")
	}
	return ""
}

func containsNumberWithUnit(value, unit string) bool {
	unit = strings.TrimSpace(unit)
	if unit == "" {
		return false
	}
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "*", "", "_", "").Replace(value)
	pattern := `[0-9０-９一二三四五六七八九十百千万两]+.{0,6}` + regexp.QuoteMeta(unit)
	return regexp.MustCompile(pattern).MatchString(compact)
}

func answerContainsNarrowQuantitativeClaim(answer, topic, unit string) bool {
	for _, paragraph := range paragraphBreakRE.Split(answer, -1) {
		if evidenceTextMatchesTopic(paragraph, topic) && containsNumberWithUnit(paragraph, unit) {
			return true
		}
	}
	return false
}

func uniqueNamedTopicConditionEvidence(topic string, refs []citationRepairEvidence) string {
	match := ""
	for _, ref := range refs {
		if !namedTopicConditionEvidence(ref.content, topic) {
			continue
		}
		if match != "" && match != ref.id {
			return ""
		}
		match = ref.id
	}
	return match
}

func namedTopicsInParagraph(paragraph string, topics []string) []string {
	normalizedParagraph := normalizedNamedTopicText(paragraph)
	seen := make(map[string]struct{}, len(topics))
	matched := make([]string, 0, 2)
	for _, topic := range topics {
		normalizedTopic := normalizedNamedTopicText(topic)
		if normalizedTopic == "" || !strings.Contains(normalizedParagraph, normalizedTopic) {
			continue
		}
		if _, exists := seen[normalizedTopic]; exists {
			continue
		}
		seen[normalizedTopic] = struct{}{}
		matched = append(matched, topic)
	}
	return matched
}

func citationEvidenceSupportsNamedTopicClaim(
	id, topic, paragraph string,
	refs []citationRepairEvidence,
) bool {
	normalizedTopic := normalizedNamedTopicText(topic)
	for _, ref := range refs {
		if ref.id != id || !strings.Contains(normalizedNamedTopicText(ref.content), normalizedTopic) {
			continue
		}
		if !paragraphClaimsApplicabilityConditions(paragraph) ||
			namedTopicConditionEvidence(ref.content, topic) {
			return true
		}
	}
	return false
}

func strongestNamedTopicEvidence(
	paragraph string,
	topic string,
	refs []citationRepairEvidence,
) string {
	normalizedTopic := normalizedNamedTopicText(topic)
	candidates := make([]citationRepairEvidence, 0, len(refs))
	conditionClaim := paragraphClaimsApplicabilityConditions(paragraph)
	for _, ref := range refs {
		if strings.Contains(normalizedNamedTopicText(ref.content), normalizedTopic) &&
			(!conditionClaim || namedTopicConditionEvidence(ref.content, topic)) {
			candidates = append(candidates, ref)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	if conditionClaim && len(candidates) == 1 {
		return candidates[0].id
	}
	if conditionClaim {
		// grep/search can register both a focused chunk and an aggregate parent
		// chunk for the same physical passage.  They are not conflicting sources.
		// Prefer the uniquely shortest fragment only when every longer candidate
		// is from that same document and contains the focused condition excerpt.
		if id := focusedSameKnowledgeConditionEvidence(topic, candidates); id != "" {
			return id
		}
	}

	claim := canonicalSourceTagRE.ReplaceAllString(paragraph, "")
	if id := unambiguousEvidenceForParagraph(claim, candidates); id != "" {
		return id
	}
	claimTokens := repairTokens(claim)
	if len(claimTokens) < 3 {
		return ""
	}
	type scored struct {
		id       string
		shared   int
		coverage float64
	}
	scores := make([]scored, 0, len(candidates))
	for _, candidate := range candidates {
		shared := sharedTokenCount(claimTokens, candidate.tokens)
		if shared < 3 {
			continue
		}
		coverage := float64(shared) / float64(len(claimTokens))
		scores = append(scores, scored{id: candidate.id, shared: shared, coverage: coverage})
	}
	if len(scores) == 0 {
		return ""
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].shared == scores[j].shared {
			if scores[i].coverage == scores[j].coverage {
				return citationOrdinal(scores[i].id) < citationOrdinal(scores[j].id)
			}
			return scores[i].coverage > scores[j].coverage
		}
		return scores[i].shared > scores[j].shared
	})
	if len(scores) > 1 && scores[0].shared-scores[1].shared < 2 &&
		scores[0].coverage-scores[1].coverage < 0.18 {
		return ""
	}
	return scores[0].id
}

func focusedSameKnowledgeConditionEvidence(topic string, candidates []citationRepairEvidence) string {
	if len(candidates) < 2 {
		return ""
	}
	knowledgeID := strings.TrimSpace(candidates[0].knowledgeID)
	if knowledgeID == "" {
		return ""
	}
	ordered := append([]citationRepairEvidence(nil), candidates...)
	for _, candidate := range ordered[1:] {
		if strings.TrimSpace(candidate.knowledgeID) != knowledgeID {
			return ""
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].contentRunes == ordered[j].contentRunes {
			return citationOrdinal(ordered[i].id) < citationOrdinal(ordered[j].id)
		}
		return ordered[i].contentRunes < ordered[j].contentRunes
	})
	// Equal-size fragments remain genuinely ambiguous; do not choose by handle.
	if ordered[0].contentRunes <= 0 || ordered[0].contentRunes == ordered[1].contentRunes {
		return ""
	}
	focusedExcerpt := normalizedNamedTopicText(conditionExcerpt(ordered[0].content, topic))
	if utf8.RuneCountInString(focusedExcerpt) < 12 {
		return ""
	}
	for _, candidate := range ordered[1:] {
		if !strings.Contains(normalizedNamedTopicText(candidate.content), focusedExcerpt) {
			return ""
		}
	}
	return ordered[0].id
}

func paragraphClaimsApplicabilityConditions(paragraph string) bool {
	return containsRepairMarker(paragraph, []string{
		"适用条件", "适宜条件", "制度条件", "适用重点", "适配点", "条件为", "条件包括",
		"applicability", "applicable condition", "conditions include",
	})
}

func paragraphReportsMissingEvidence(paragraph string) bool {
	return containsRepairMarker(paragraph, []string{
		"未在检索信息中找到", "未在检索结果中找到", "未找到直接", "没有找到直接",
		"暂无直接", "未取得直接", "not found in the retrieved", "no direct evidence found",
	})
}

func containsRepairMarker(value string, markers []string) bool {
	value = strings.ToLower(value)
	for _, marker := range markers {
		if strings.Contains(value, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func namedTopicConditionEvidence(content, topic string) bool {
	compactTopic := normalizedNamedTopicText(topic)
	if compactTopic == "" {
		return false
	}
	markers := []string{
		"应同时满足下列条件", "符合下列特定条件之一", "符合下列条件之一",
		"适宜采用", "适用于", "适用条件", "条件包括", "applicableconditions", "conditionsinclude",
	}
	// Bind the condition marker and method name inside the same sentence. This
	// prevents a chunk ending with a definition of method A and beginning the
	// applicability conditions of neighboring method B from qualifying for A.
	for _, clause := range strings.FieldsFunc(content, func(r rune) bool {
		switch r {
		case '。', '！', '!', '？', '?', '\n', '\r':
			return true
		default:
			return false
		}
	}) {
		compactClause := normalizedNamedTopicText(clause)
		if !strings.Contains(compactClause, compactTopic) {
			continue
		}
		topicPositions := allStringIndexes(compactClause, compactTopic)
		for _, marker := range markers {
			marker = normalizedNamedTopicText(marker)
			for _, markerAt := range allStringIndexes(compactClause, marker) {
				for _, topicAt := range topicPositions {
					if absInt(markerAt-topicAt) <= 360 {
						return true
					}
				}
			}
		}
	}
	return false
}

func allStringIndexes(value, target string) []int {
	if target == "" {
		return nil
	}
	out := make([]int, 0, 2)
	for start := 0; start < len(value); {
		index := strings.Index(value[start:], target)
		if index < 0 {
			break
		}
		index += start
		out = append(out, index)
		start = index + len(target)
	}
	return out
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func namedTopicConditionExcerpt(id, topic string, refs []citationRepairEvidence) string {
	for _, ref := range refs {
		if ref.id != id || !namedTopicConditionEvidence(ref.content, topic) {
			continue
		}
		return conditionExcerpt(ref.content, topic)
	}
	return ""
}

func conditionExcerpt(content, topic string) string {
	value := strings.Join(strings.Fields(content), " ")
	lower := strings.ToLower(value)
	topicAt := strings.Index(lower, strings.ToLower(topic))
	if topicAt < 0 {
		return ""
	}
	best := -1
	bestDistance := int(^uint(0) >> 1)
	for _, marker := range []string{
		"应同时满足下列条件", "符合下列特定条件之一", "符合下列条件之一",
		"适宜采用", "适用于", "适用条件", "条件包括",
	} {
		for start := 0; start < len(lower); {
			index := strings.Index(lower[start:], strings.ToLower(marker))
			if index < 0 {
				break
			}
			index += start
			distance := absInt(index - topicAt)
			if distance < bestDistance {
				best = index
				bestDistance = distance
			}
			start = index + len(marker)
		}
	}
	if best < 0 {
		return ""
	}
	start := best
	if colon := strings.IndexAny(value[start:], "：:"); colon >= 0 && colon <= 80 {
		colonAt := start + colon
		_, width := utf8.DecodeRuneInString(value[colonAt:])
		start = colonAt + width
	}
	tail := strings.TrimSpace(value[start:])
	if tail == "" {
		return ""
	}
	runes := []rune(tail)
	if len(runes) > 240 {
		cut := 240
		for index := 239; index >= 140; index-- {
			if strings.ContainsRune("；;。", runes[index]) {
				cut = index + 1
				break
			}
		}
		tail = string(runes[:cut])
	}
	return strings.Trim(strings.TrimSpace(tail), "；;。 ")
}

func renderGroundedConditionParagraph(paragraph, topic, excerpt, citationID string) string {
	topicAt := strings.Index(paragraph, topic)
	if topicAt < 0 {
		return paragraph
	}
	prefixEnd := -1
	if colon := strings.IndexAny(paragraph[topicAt+len(topic):], "：:"); colon >= 0 && colon <= 24 {
		colonAt := topicAt + len(topic) + colon
		_, width := utf8.DecodeRuneInString(paragraph[colonAt:])
		prefixEnd = colonAt + width
	}
	if prefixEnd < 0 || prefixEnd > len(paragraph) {
		return paragraph
	}
	suffix := ""
	for _, marker := range []string{"；该直接条件", ";该直接条件", "。该直接条件"} {
		if index := strings.Index(paragraph[prefixEnd:], marker); index >= 0 {
			suffix = paragraph[prefixEnd+index:]
			break
		}
	}
	if suffix == "" {
		suffix = "。"
	}
	return strings.TrimRight(paragraph[:prefixEnd], " \t") + "制度直接适用条件包括：" +
		strings.TrimSpace(excerpt) + canonicalCitationTag(citationID) + suffix
}

func replaceCitationID(value, currentID, replacementID string) string {
	return canonicalSourceTagRE.ReplaceAllStringFunc(value, func(tag string) string {
		match := canonicalSourceTagRE.FindStringSubmatch(tag)
		if len(match) == 2 && match[1] == currentID {
			return canonicalCitationTag(replacementID)
		}
		return tag
	})
}

func normalizedNamedTopicText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, value)
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
		// A claim paragraph may end with an inline source title. It is not a
		// source-only paragraph, and treating it as one can rotate citations
		// backward across neighboring option definitions. Keep short prefixes
		// such as "根据" or "来源：" eligible, but reject substantive prose
		// before the first document title.
		if titleAt := strings.Index(probe, "《"); titleAt > 0 {
			prefix := strings.Trim(strings.TrimSpace(probe[:titleAt]), "*_`#> 📄📚🔗：:。.;；")
			if utf8.RuneCountInString(prefix) > 12 &&
				!strings.HasPrefix(prefix, "来源") &&
				!strings.HasPrefix(prefix, "出处") &&
				!strings.HasPrefix(strings.ToLower(prefix), "source") {
				return false
			}
		}
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
		segment = normalize(parenthesizedSrcCitationAliasRE, segment)
		segment = normalize(angleCitationAliasRE, segment)
		return normalize(srcCitationAliasRE, segment)
	})
}

type citationRepairEvidence struct {
	id           string
	knowledgeID  string
	content      string
	contentRunes int
	tokens       map[string]struct{}
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
		normalizedContent := normalizedRepairText(content)
		result = append(result, citationRepairEvidence{
			id:           id,
			knowledgeID:  strings.TrimSpace(ref.KnowledgeID),
			content:      normalizedContent,
			contentRunes: utf8.RuneCountInString(normalizedContent),
			tokens:       repairTokens(content),
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
