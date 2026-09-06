// Package chunker - header_tracker.go implements context-preserving header
// tracking for document chunking, ported from docreader/splitter/header_hook.py.
//
// When a large Markdown table is split across multiple chunks, each chunk after
// the first would lose the table header context. The headerTracker detects table
// headers and signals the merge logic to prepend them to subsequent chunks.
package chunker

import (
	"regexp"
	"sort"
	"strings"
)

// headerTrackerHook defines a pattern pair for detecting contextual headers.
// When startPattern matches a unit's text, that text becomes an "active header".
// The header stays active until endPattern matches a subsequent unit.
type headerTrackerHook struct {
	startPattern *regexp.Regexp
	endPattern   *regexp.Regexp
	priority     int
}

// defaultHeaderHooks returns header tracking hooks matching the Python defaults
// in docreader/splitter/header_hook.py.
var defaultHeaderHooks = []headerTrackerHook{
	{
		// Markdown table: header row + separator row (e.g. "| A | B |\n| --- | --- |\n")
		startPattern: regexp.MustCompile(`(?si)^\s*(?:\|[^|\n]*)+[\r\n]+\s*(?:\|\s*:?-{3,}:?\s*)+\|?[\r\n]+$`),
		// Empty/whitespace line or a line that doesn't start with | or whitespace
		endPattern: regexp.MustCompile(`(?si)^\s*$|^\s*[^|\s].*$`),
		priority:   15,
	},
}

// tableRowPattern matches a single Markdown table row: "| cell | cell | ... |\n"
var tableRowPattern = regexp.MustCompile(`(?m)^\s*(?:\|[^|\n]*)+\|\s*$`)

// markdownTableHookPriority matches DEFAULT_CONFIGS / defaultHeaderHooks table hook.
const markdownTableHookPriority = 15

// headerTracker maintains the state of active headers across split units.
type headerTracker struct {
	hooks         []headerTrackerHook
	activeHeaders map[int]string // priority -> header text
	endedHeaders  map[int]bool   // priorities that have been ended
	// pendingTableBreak is set when a table row unit ends with a paragraph break
	// (the blank line between tables is consumed by \n\n splitting). The header
	// stays active until the next unit is seen so we can detect a new table.
	pendingTableBreak bool
	// headerEndedThisUnit tells mergeUnits to flush before the current unit when a
	// new table starts (column mismatch or pendingTableBreak + table row).
	headerEndedThisUnit bool
}

func newHeaderTracker() *headerTracker {
	return &headerTracker{
		hooks:         defaultHeaderHooks,
		activeHeaders: make(map[int]string),
		endedHeaders:  make(map[int]bool),
	}
}

// update checks split text for header start/end markers and updates internal state.
func (ht *headerTracker) update(split string) {
	ht.headerEndedThisUnit = false

	if ht.pendingTableBreak {
		ht.pendingTableBreak = false
		if _, active := ht.activeHeaders[markdownTableHookPriority]; active {
			if firstTableRowColumnCount(split) > 0 {
				ht.clearTableHeader()
				ht.headerEndedThisUnit = true
			} else {
				ht.clearTableHeader()
			}
		}
	}

	// 1. Check for header-end markers among currently active headers
	for _, hook := range ht.hooks {
		if _, active := ht.activeHeaders[hook.priority]; active {
			if hook.endPattern.MatchString(split) {
				ht.endedHeaders[hook.priority] = true
				delete(ht.activeHeaders, hook.priority)
			}
		}
	}

	// 1b. Paragraph splits consume the blank line between tables. Mark a break
	// after "| last row |\n\n" and resolve on the next unit; also end when a new
	// table row has a different column count than the active header.
	if _, active := ht.activeHeaders[markdownTableHookPriority]; active {
		{
			if splitEndsWithParagraphBreak(split) {
				ht.pendingTableBreak = true
			} else {
				ht.endTableHeaderOnColumnMismatch(split)
			}
		}
	}

	// 3. Check for new header-start markers (only for hooks that are neither active nor ended)
	for _, hook := range ht.hooks {
		if _, active := ht.activeHeaders[hook.priority]; active {
			continue
		}
		if ht.endedHeaders[hook.priority] {
			continue
		}
		if loc := hook.startPattern.FindString(split); loc != "" {
			ht.activeHeaders[hook.priority] = loc
		}
	}

	// 4. If all headers ended, clear the ended set so future tables can be tracked
	if len(ht.activeHeaders) == 0 {
		for k := range ht.endedHeaders {
			delete(ht.endedHeaders, k)
		}
	}
}

// getHeaders returns all active headers concatenated, sorted by priority descending.
func (ht *headerTracker) getHeaders() string {
	if len(ht.activeHeaders) == 0 {
		return ""
	}

	type entry struct {
		priority int
		text     string
	}
	entries := make([]entry, 0, len(ht.activeHeaders))
	for p, t := range ht.activeHeaders {
		entries = append(entries, entry{p, t})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].priority > entries[j].priority
	})

	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = e.text
	}
	return strings.Join(parts, "\n")
}

func (ht *headerTracker) clearTableHeader() {
	ht.endedHeaders[markdownTableHookPriority] = true
	delete(ht.activeHeaders, markdownTableHookPriority)
}

func (ht *headerTracker) endTableHeaderOnColumnMismatch(split string) {
	header, ok := ht.activeHeaders[markdownTableHookPriority]
	if !ok {
		return
	}
	rowCols := firstTableRowColumnCount(split)
	headerCols := headerTableColumnCount(header)
	if rowCols > 0 && headerCols > 0 && rowCols != headerCols {
		ht.clearTableHeader()
		ht.headerEndedThisUnit = true
	}
}

func splitEndsWithParagraphBreak(split string) bool {
	trimmed := strings.TrimRight(split, " \t\r")
	return strings.HasSuffix(trimmed, "\n\n") || strings.HasSuffix(trimmed, "\r\n\r\n")
}

func tableRowColumnCount(line string) int {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") {
		return 0
	}
	parts := strings.Split(line, "|")
	if len(parts) > 0 && strings.TrimSpace(parts[0]) == "" {
		parts = parts[1:]
	}
	if len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	return len(parts)
}

func firstTableRowColumnCount(text string) int {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && tableRowPattern.MatchString(line) {
			return tableRowColumnCount(line)
		}
	}
	return 0
}

func headerTableColumnCount(header string) int {
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "---") {
			continue
		}
		if n := tableRowColumnCount(line); n > 0 {
			return n
		}
	}
	return 0
}

// headerColumnMismatch reports whether the next split unit starts a new table
// whose width differs from the active markdown table header.
func headerColumnMismatch(headers, nextUnit string) bool {
	headerCols := headerTableColumnCount(headers)
	rowCols := firstTableRowColumnCount(nextUnit)
	return headerCols > 0 && rowCols > 0 && headerCols != rowCols
}
