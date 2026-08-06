// Package questioncoverage classifies whether an enrichment source record
// contains enough real information to require a generated question. It is a
// deliberately conservative filter: only unmistakable placeholders and
// structurally empty table fragments are exempt from coverage.
package questioncoverage

import (
	"strings"
	"unicode"
)

type Assessment struct {
	Eligible bool
	Reason   string
}

func Assess(content string) Assessment {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return Assessment{Reason: "empty"}
	}
	lower := strings.ToLower(trimmed)
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, lower)
	for _, placeholder := range []string{
		"空白", "空白模板", "空白表单", "暂无", "暂无内容", "无内容",
		"无正文", "略", "nil", "none", "n/a", "na",
	} {
		if compact == strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
				return -1
			}
			return unicode.ToLower(r)
		}, placeholder) {
			return Assessment{Reason: "placeholder"}
		}
	}

	alnum := 0
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			alnum++
		}
	}
	if alnum < 2 {
		return Assessment{Reason: "too_few_information_characters"}
	}

	// Markdown tables made only of delimiters, headings such as "列1", and
	// blank cells do not justify a question. Any ordinary prose remains
	// eligible even when short, so legitimate clauses are never discarded by
	// this local heuristic.
	lines := strings.Split(trimmed, "\n")
	tableLines, informativeCells := 0, 0
	for _, line := range lines {
		if strings.Count(line, "|") < 2 {
			continue
		}
		tableLines++
		for _, cell := range strings.Split(line, "|") {
			cell = strings.TrimSpace(cell)
			if cell == "" || strings.Trim(cell, "-: ") == "" {
				continue
			}
			cellRunes := 0
			for _, r := range cell {
				if unicode.IsLetter(r) || unicode.IsDigit(r) {
					cellRunes++
				}
			}
			if cellRunes >= 1 {
				informativeCells++
			}
		}
	}
	if tableLines > 0 && tableLines == len(lines) && informativeCells == 0 {
		return Assessment{Reason: "structurally_empty_table"}
	}
	return Assessment{Eligible: true, Reason: "substantive"}
}

type Report struct {
	Eligible           int
	LowInformation     int
	InitialMissing     int
	InitialEmpty       int
	Recovered          int
	UnresolvedEligible int
}
