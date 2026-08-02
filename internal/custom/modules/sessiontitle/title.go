package sessiontitle

import (
	"regexp"
	"strings"
	"unicode"
)

const maxTitleRunes = 32

var (
	thinkBlockRE = regexp.MustCompile(`(?is)<think\b[^>]*>.*?</think\s*>`)
	labelRE      = regexp.MustCompile(`(?i)^\s*(?:title|标题|会话标题)\s*[:：]\s*`)
	spaceRE      = regexp.MustCompile(`\s+`)
	protocolRE   = regexp.MustCompile(`(?is)<src\b[^>]*>|\[/?(?:AVAILABLE_CITATIONS|WEKNORA_CITATION_OUTPUT)\]`)
)

// NormalizeModelTitle accepts plain model text only. Empty/thinking-only
// responses remain empty so callers can use the deterministic fallback without
// issuing a second model request.
func NormalizeModelTitle(raw string) string {
	value := thinkBlockRE.ReplaceAllString(raw, " ")
	value = protocolRE.ReplaceAllString(value, " ")
	value = strings.TrimSpace(value)
	value = labelRE.ReplaceAllString(value, "")
	value = strings.Trim(value, "`#*_~\"'“”‘’[]()（） ")
	if index := strings.IndexAny(value, "\r\n"); index >= 0 {
		value = value[:index]
	}
	value = spaceRE.ReplaceAllString(strings.TrimSpace(value), " ")
	value = strings.TrimRightFunc(value, func(r rune) bool {
		return unicode.IsPunct(r) && r != '?' && r != '？'
	})
	return truncateRunes(value, maxTitleRunes)
}

// Fallback derives a compact non-empty title locally from the first user
// message. It is fault containment for empty/error model responses, not a
// second generation or a compatibility repair.
func Fallback(query string) string {
	value := protocolRE.ReplaceAllString(query, " ")
	value = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(value)
	value = spaceRE.ReplaceAllString(strings.TrimSpace(value), " ")
	value = strings.Trim(value, "`#*_-~\"'“”‘’[]()（） ")
	value = strings.TrimRightFunc(value, unicode.IsPunct)
	value = truncateRunes(value, maxTitleRunes)
	if value == "" {
		return "新会话"
	}
	return value
}

func truncateRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return strings.TrimSpace(string(runes[:maximum]))
}
