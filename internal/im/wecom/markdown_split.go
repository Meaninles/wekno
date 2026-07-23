package wecom

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const wecomMarkdownMaxBytes = 2048

// splitWeComMarkdown splits an application Markdown message into payloads that
// fit WeCom's 2048-byte UTF-8 limit. It prefers paragraph, line, sentence, and
// whitespace boundaries, and only falls back to a raw UTF-8 rune boundary when
// a single segment is too long.
func splitWeComMarkdown(content string) []string {
	if len(content) <= wecomMarkdownMaxBytes {
		return []string{content}
	}

	// The prefix size depends on the final number of chunks. Recalculate until
	// the byte budget stabilizes (normally two iterations).
	bodyLimit := wecomMarkdownMaxBytes
	for {
		bodies := splitWeComMarkdownBodies(content, bodyLimit)
		prefixBytes := len(wecomMarkdownPartPrefix(len(bodies), len(bodies)))
		nextBodyLimit := wecomMarkdownMaxBytes - prefixBytes
		if nextBodyLimit == bodyLimit {
			chunks := make([]string, len(bodies))
			for i, body := range bodies {
				chunks[i] = wecomMarkdownPartPrefix(i+1, len(bodies)) + body
			}
			return chunks
		}
		bodyLimit = nextBodyLimit
	}
}

func wecomMarkdownPartPrefix(part, total int) string {
	return fmt.Sprintf("（%d/%d）\n", part, total)
}

func splitWeComMarkdownBodies(content string, maxBytes int) []string {
	if maxBytes <= 0 {
		return []string{content}
	}

	chunks := make([]string, 0, len(content)/maxBytes+1)
	for len(content) > maxBytes {
		cut := preferredWeComMarkdownCut(content, maxBytes)
		chunks = append(chunks, content[:cut])
		content = content[cut:]
	}
	if content != "" || len(chunks) == 0 {
		chunks = append(chunks, content)
	}
	return chunks
}

func preferredWeComMarkdownCut(content string, maxBytes int) int {
	if len(content) <= maxBytes {
		return len(content)
	}

	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	if cut == 0 {
		_, size := utf8.DecodeRuneInString(content)
		return size
	}

	prefix := content[:cut]
	minPreferred := cut / 2

	if idx := strings.LastIndex(prefix, "\n\n"); idx >= minPreferred {
		return idx + 2
	}
	if idx := strings.LastIndex(prefix, "\n"); idx >= minPreferred {
		return idx + 1
	}
	if idx := strings.LastIndexAny(prefix, "。！？；.!?;"); idx >= minPreferred {
		_, size := utf8.DecodeRuneInString(prefix[idx:])
		return idx + size
	}
	if idx := strings.LastIndexAny(prefix, " \t"); idx >= minPreferred {
		return idx + 1
	}

	return cut
}
