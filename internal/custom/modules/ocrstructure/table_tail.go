// Package ocrstructure detects output-only structure that carries no OCR
// information. It never removes a cell containing a letter or number.
package ocrstructure

import (
	"strings"
	"unicode"
)

const runawayEmptyTableLines = 8

// HasRunawayEmptyTableTail reports whether a streaming response is spending
// its token budget on consecutive Markdown rows with no recognizable cell
// content. This catches tails such as "| | |" that evade text-repetition
// guards because they contain almost no letters or numbers.
func HasRunawayEmptyTableTail(text string) bool {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	consecutive := 0
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		if emptyTableLine(line) {
			consecutive++
			if consecutive >= runawayEmptyTableLines {
				return true
			}
			continue
		}
		break
	}
	return false
}

// TrimEmptyTableTail removes only a trailing run of structurally empty table
// rows. No row containing a letter or number is ever removed.
func TrimEmptyTableTail(text string) (string, int) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	start := end
	for start > 0 && emptyTableLine(strings.TrimSpace(lines[start-1])) {
		start--
	}
	removed := end - start
	if removed < 3 {
		return strings.TrimSpace(text), 0
	}
	return strings.TrimSpace(strings.Join(lines[:start], "\n")), removed
}

func emptyTableLine(line string) bool {
	if strings.Count(line, "|") < 2 {
		return false
	}
	for _, r := range line {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return false
		}
	}
	return true
}
