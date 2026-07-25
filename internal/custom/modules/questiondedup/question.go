// Package questiondedup provides generation-scoped normalization and durable
// claim models for generated retrieval questions. The database repository owns
// the actual claim transaction; this package deliberately contains no service
// or repository dependencies.
package questiondedup

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxQuestionRunes            = 240
	MaxNormalizedRunes          = 512
	superficialSimilarityPermil = 940
	minFuzzyComparisonRunes     = 12
)

var (
	numberingPrefix = regexp.MustCompile(`^\s*(?:[-*•●▪◦]+|\(?\d{1,4}\)?[.、):：-]|[一二三四五六七八九十百千]+[、.）)])\s*`)
	spaceRuns       = regexp.MustCompile(`\s+`)
	// These phrases expose the mechanics of source slicing or question
	// generation instead of expressing a real user information need.
	generationMetadata = regexp.MustCompile(
		`(?i)(?:^\s*(?:根据|依据|参照|按照)\s*《[^》]+》|` +
			`^\s*(?:根据|依据|参照)\s*[^，。！？?]{0,40}(?:制度|规定|办法|细则)(?:中|的|，|,)|` +
			`原文件\s*第|原文\s*第|第\s*(?:\d+|[一二三四五六七八九十百千]+)\s*(?:页|组|段|章|节|款|项|部分|chunk|分片)|` +
			`第\s*(?:\d+|[一二三四五六七八九十百千]+)\s*条\s*(?:规定的?)?\s*(?:内容|要求|是什么|有哪些)|` +
			`(?:根据|参照)\s*(?:第\s*(?:\d+|[一二三四五六七八九十百千]+)\s*(?:页|组|段)|上述(?:文档|片段|内容)|本(?:文|段|片段))|` +
			`制度原文(?:中|里)?|(?:该|此|上述)\s*(?:文档|片段|chunk)\s*(?:中|里)?)`,
	)
	sourceFileName = regexp.MustCompile(
		`(?i)[^\s，。！？?]{1,160}\.(?:pdf|docx?|xlsx?|pptx?|epub|mhtml|markdown|md|txt|text|csv|json)`,
	)
)

type Claim struct {
	TenantID             uint64    `gorm:"primaryKey;column:tenant_id;uniqueIndex:idx_generated_question_claim_slot"`
	KnowledgeID          string    `gorm:"primaryKey;column:knowledge_id;uniqueIndex:idx_generated_question_claim_slot"`
	KnowledgeBaseID      string    `gorm:"column:knowledge_base_id;not null"`
	ProcessingGeneration string    `gorm:"primaryKey;column:processing_generation;uniqueIndex:idx_generated_question_claim_slot"`
	QuestionHash         string    `gorm:"primaryKey;column:question_hash;size:64"`
	ClaimID              string    `gorm:"column:claim_id;not null;size:256;uniqueIndex:idx_generated_question_claim_slot"`
	Question             string    `gorm:"column:question;not null"`
	NormalizedQuestion   string    `gorm:"column:normalized_question;not null"`
	CreatedAt            time.Time `gorm:"column:created_at;not null"`
}

func (Claim) TableName() string {
	return "custom_generated_question_claims"
}

type Candidate struct {
	ClaimID            string
	Question           string
	NormalizedQuestion string
	QuestionHash       string
}

// Prepare removes presentation numbering, rejects generation/source-location
// leakage and returns a stable semantic hash. It intentionally keeps the
// displayed wording natural while normalization ignores punctuation,
// whitespace and case for duplicate detection across concurrent batches.
func Prepare(claimID, raw string) (Candidate, bool) {
	claimID = strings.TrimSpace(claimID)
	question := strings.TrimSpace(numberingPrefix.ReplaceAllString(raw, ""))
	question = strings.TrimSpace(strings.Trim(question, `"'“”‘’`))
	if claimID == "" || question == "" || utf8.RuneCountInString(question) < 6 ||
		utf8.RuneCountInString(question) > MaxQuestionRunes ||
		generationMetadata.MatchString(question) ||
		sourceFileName.MatchString(question) {
		return Candidate{}, false
	}
	normalized := Normalize(question)
	if normalized == "" || utf8.RuneCountInString(normalized) > MaxNormalizedRunes {
		return Candidate{}, false
	}
	digest := sha256.Sum256([]byte(normalized))
	return Candidate{
		ClaimID:            claimID,
		Question:           ensureQuestionMark(question),
		NormalizedQuestion: normalized,
		QuestionHash:       hex.EncodeToString(digest[:]),
	}, true
}

func Normalize(question string) string {
	question = strings.ToLower(spaceRuns.ReplaceAllString(strings.TrimSpace(question), " "))
	var builder strings.Builder
	builder.Grow(len(question))
	for _, r := range question {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// IsSuperficialParaphrase catches high-overlap questions that differ only by
// one source-section number, punctuation-sized wording changes, or another
// tiny edit. Exact hashes remain the cheap first line of defence; this bounded
// edit-distance check is used while a generation-scoped claim lock is held so
// concurrent batches cannot both accept the same stem.
func IsSuperficialParaphrase(first, second string) bool {
	firstRunes := []rune(first)
	secondRunes := []rune(second)
	if len(firstRunes) == 0 || len(secondRunes) == 0 {
		return false
	}
	if first == second {
		return true
	}
	maxLength := max(len(firstRunes), len(secondRunes))
	minLength := min(len(firstRunes), len(secondRunes))
	if minLength < minFuzzyComparisonRunes {
		return false
	}
	maxDistance := maxLength * (1000 - superficialSimilarityPermil) / 1000
	if maxDistance < 1 {
		maxDistance = 1
	}
	if maxLength-minLength > maxDistance {
		return false
	}
	return editDistanceWithin(firstRunes, secondRunes, maxDistance)
}

func editDistanceWithin(first, second []rune, limit int) bool {
	if len(first) > len(second) {
		first, second = second, first
	}
	if len(second)-len(first) > limit {
		return false
	}
	const unreachablePadding = 1
	unreachable := limit + unreachablePadding
	previous := make([]int, len(second)+1)
	for column := range previous {
		if column <= limit {
			previous[column] = column
		} else {
			previous[column] = unreachable
		}
	}
	for row := 1; row <= len(first); row++ {
		current := make([]int, len(second)+1)
		for column := range current {
			current[column] = unreachable
		}
		if row <= limit {
			current[0] = row
		}
		start := max(1, row-limit)
		end := min(len(second), row+limit)
		for column := start; column <= end; column++ {
			substitutionCost := 0
			if first[row-1] != second[column-1] {
				substitutionCost = 1
			}
			current[column] = min(
				previous[column]+1,
				current[column-1]+1,
				previous[column-1]+substitutionCost,
			)
		}
		previous = current
	}
	return previous[len(second)] <= limit
}

func ensureQuestionMark(question string) string {
	question = strings.TrimSpace(question)
	if question == "" {
		return ""
	}
	last, _ := utf8.DecodeLastRuneInString(question)
	switch last {
	case '?', '？':
		return question
	}
	for _, r := range question {
		if unicode.In(r, unicode.Han) {
			return strings.TrimRight(question, ".。!！;；") + "？"
		}
	}
	return strings.TrimRight(question, ".!;") + "?"
}
