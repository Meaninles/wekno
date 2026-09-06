package chunker

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
)

var sourceSection = regexp.MustCompile(`^第[〇零一二三四五六七八九十百千万两0-9]+[章节条]`)

// Numbered paragraphs are structural ancestors too. Their literal text can
// name the subject or condition governing child items even when the source
// does not mark them as Markdown/Word headings.
var sourceListLevels = []struct {
	pattern *regexp.Regexp
	level   int
}{
	{regexp.MustCompile(`^[〇零一二三四五六七八九十百千万两]+[、．.]\s*\S`), 7},
	{regexp.MustCompile(`^[（(][〇零一二三四五六七八九十百千万两]+[）)]\s*\S`), 8},
	{regexp.MustCompile(`^[0-9]+[、．.]\s*[^0-9\s]`), 9},
	{regexp.MustCompile(`^[（(][0-9]+[）)]\s*\S`), 10},
	{regexp.MustCompile(`^[a-zA-Z][.)]\s+\S`), 11},
}

type sourceLine struct {
	start, end          int
	body, table, header string
	headings            []string
}

// annotateStructure records coordinates in the parsed source, explicitly
// distinct from physical page/sheet positions. No chunk ordinal is promoted
// into an article number. Content remains a literal source slice.
func annotateStructure(text string, chunks []Chunk) []Chunk {
	if len(chunks) == 0 {
		return chunks
	}
	digest := sha256.Sum256([]byte(text))
	var lines []sourceLine
	offset := 0
	headings := []string{}
	levels := []int{}
	pushHeading := func(level int, title string) {
		for len(levels) > 0 && levels[len(levels)-1] >= level {
			levels = levels[:len(levels)-1]
			headings = headings[:len(headings)-1]
		}
		levels = append(levels, level)
		headings = append(headings, title)
	}
	table, header := "", ""
	sourceLines := strings.SplitAfter(text, "\n")
	for lineIndex, body := range sourceLines {
		trimmed := strings.TrimSpace(body)
		if match := MarkdownHeadingPattern.FindStringSubmatch(trimmed); len(match) > 0 {
			level := len(match[1])
			pushHeading(level, trimmed)
		} else if label := sourceSection.FindString(trimmed); label != "" {
			level := 3
			if strings.HasSuffix(label, "章") {
				level = 1
			} else if strings.HasSuffix(label, "节") {
				level = 2
			}
			pushHeading(level, trimmed)
		} else {
			for _, item := range sourceListLevels {
				if item.pattern.MatchString(trimmed) {
					pushHeading(item.level, trimmed)
					break
				}
			}
		}
		isRow := strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2
		if isRow {
			if table == "" {
				table = fmt.Sprintf("table:%x:%d", digest[:8], offset)
				header = ""
				if lineIndex+1 < len(sourceLines) && isMarkdownSeparator(sourceLines[lineIndex+1]) {
					for _, cell := range tableCells(trimmed) {
						if strings.TrimSpace(cell) != "" {
							header = trimmed
							break
						}
					}
				}
			}
		} else {
			table = ""
			header = ""
		}
		end := offset + utf8.RuneCountInString(body)
		lines = append(lines, sourceLine{offset, end, body, table, header, append([]string(nil), headings...)})
		offset = end
	}
	for i := range chunks {
		c := &chunks[i]
		if c.Start < 0 || c.End <= c.Start || c.End > offset {
			continue
		}
		firstContent := c.Start + utf8.RuneCountInString(c.Content) - utf8.RuneCountInString(strings.TrimLeft(c.Content, " \t\r\n"))
		first := sort.Search(len(lines), func(i int) bool { return lines[i].end > firstContent })
		last := sort.Search(len(lines), func(i int) bool { return lines[i].start >= c.End })
		if first >= len(lines) {
			continue
		}
		line := lines[first]
		parts := []string{}
		parts = append(parts, line.headings...)
		locator := map[string]any{"kind": "parsed_text", "parsed_start": c.Start, "parsed_end": c.End, "block_id": fmt.Sprintf("block:%x:%d:%d", digest[:8], c.Start, c.End), "heading_path": line.headings}
		var tables []map[string]any
		for j := first; j < last; j++ {
			l := lines[j]
			if l.table == "" {
				continue
			}
			if l.header != "" && !strings.Contains(c.Content, l.header) && !strings.Contains(strings.Join(parts, "\n"), l.header) {
				parts = append(parts, l.header)
			}
			key := ""
			cells := tableCells(l.body)
			if len(cells) > 0 {
				key = cells[0]
			}
			continued := c.Start > l.start || c.End < l.end
			row := map[string]any{"table_id": l.table, "parsed_row_start": l.start, "row_key": key, "header": l.header, "continuation": continued}
			tables = append(tables, row)
			if continued && key != "" && !strings.Contains(c.Content, key) {
				parts = append(parts, "Row key (continued): "+key)
			}
		}
		if len(tables) > 0 {
			locator["table_rows"] = tables
		}
		c.SourceLocator, _ = json.Marshal(locator)
		header := strings.TrimSpace(strings.Join(parts, "\n"))
		if len(line.headings) > 0 {
			// Absolute source positions are authoritative. A parent may span
			// several sibling sections; concatenating its earlier breadcrumb
			// with the child's path both duplicates context and assigns the
			// previous section to a later chunk.
			c.ContextHeader = header
		} else if header != "" {
			c.ContextHeader = mergeBreadcrumbs(header, c.ContextHeader)
		}
	}
	return chunks
}

func tableCells(row string) []string {
	var out []string
	var part strings.Builder
	escaped := false
	for _, r := range strings.TrimSpace(row) {
		if r == '|' && !escaped {
			out = append(out, strings.TrimSpace(part.String()))
			part.Reset()
		} else {
			part.WriteRune(r)
		}
		if r == '\\' && !escaped {
			escaped = true
		} else {
			escaped = false
		}
	}
	if part.Len() > 0 {
		out = append(out, strings.TrimSpace(part.String()))
	}
	if len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	return out
}

// MergeSourceLocator preserves a parser-provided physical coordinate and adds
// the parsed structure without pretending the two coordinate systems coincide.
func MergeSourceLocator(physical, structure types.JSON) types.JSON {
	if len(physical) == 0 {
		return structure
	}
	if len(structure) == 0 {
		return physical
	}
	var p, s map[string]any
	if json.Unmarshal(physical, &p) != nil || json.Unmarshal(structure, &s) != nil {
		return physical
	}
	p["parsed_structure"] = s
	out, _ := json.Marshal(p)
	return out
}

var markdownSeparatorCell = regexp.MustCompile(`^:?-{3,}:?$`)

func isMarkdownSeparator(line string) bool {
	cells := tableCells(line)
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		if !markdownSeparatorCell.MatchString(cell) {
			return false
		}
	}
	return true
}
