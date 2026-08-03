package wecom

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const wecomMarkdownMaxBytes = 2048

var weComMarkdownLinkRE = regexp.MustCompile(`\[(?:\\.|[^\]\r\n])*\]\((?:\\.|[^)\r\n])*\)`)

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

// SplitApplicationMarkdown exposes the exact application-message partitioning
// used by SendReply. The custom read-only preview API calls this function so a
// simulated payload cannot drift from the real WeCom application transport.
func SplitApplicationMarkdown(content string) []string {
	return splitWeComMarkdown(content)
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
		return avoidWeComMarkdownLinkCut(content, idx+2, maxBytes)
	}
	if idx := strings.LastIndex(prefix, "\n"); idx >= minPreferred {
		return avoidWeComMarkdownLinkCut(content, idx+1, maxBytes)
	}
	if idx := strings.LastIndexAny(prefix, "。！？；.!?;"); idx >= minPreferred {
		_, size := utf8.DecodeRuneInString(prefix[idx:])
		return avoidWeComMarkdownLinkCut(content, idx+size, maxBytes)
	}
	if idx := strings.LastIndexAny(prefix, " \t"); idx >= minPreferred {
		return avoidWeComMarkdownLinkCut(content, idx+1, maxBytes)
	}

	return avoidWeComMarkdownLinkCut(content, cut, maxBytes)
}

// avoidWeComMarkdownLinkCut treats each inline Markdown link as one transport
// token. Normal references are much smaller than the platform limit, so moving
// the boundary to the start of the link preserves clickability without adding
// another message or model pass.
func avoidWeComMarkdownLinkCut(content string, cut, maxBytes int) int {
	if cut <= 0 || cut >= len(content) {
		return cut
	}
	for _, span := range weComMarkdownLinkRE.FindAllStringIndex(content, -1) {
		if span[0] >= cut {
			break
		}
		if span[0] < cut && cut < span[1] {
			if span[0] > 0 {
				return span[0]
			}
			if span[1] <= maxBytes {
				return span[1]
			}
			// A single link longer than WeCom's entire payload limit cannot be
			// kept atomic. Retain the UTF-8-safe transport boundary.
			return cut
		}
	}
	return cut
}
