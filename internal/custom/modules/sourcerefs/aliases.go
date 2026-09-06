package sourcerefs

import (
	"github.com/Tencent/WeKnora/internal/types"
	"regexp"
	"strings"
)

var (
	groupedCitationAliasRE          = regexp.MustCompile(`[\(（\[【]\s*(S[1-9][0-9]*(?:\s*[,，、;；]\s*S[1-9][0-9]*)+)\s*[\)）\]】]`)
	groupedCitationIDRE             = regexp.MustCompile(`S[1-9][0-9]*`)
	plainCitationAliasRE            = regexp.MustCompile(`[\(（\[【]\s*(S[1-9][0-9]*)\s*[\)）\]】]`)
	parenthesizedSrcCitationAliasRE = regexp.MustCompile(`(?i)[\(（\[【]\s*src\s+id\s*=\s*["']?(S[1-9][0-9]*)["']?\s*[\)）\]】]`)
	angleCitationAliasRE            = regexp.MustCompile(`<\s*(S[1-9][0-9]*)\s*(?:/\s*)?>`)
	srcCitationAliasRE              = regexp.MustCompile(`(?i)<\s*src\s+id\s*=\s*["'](S[1-9][0-9]*)["']\s*/?\s*>`)
)

// Normalize only explicit handles already present in the evidence registry.
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
		segment = groupedCitationAliasRE.ReplaceAllStringFunc(segment, func(alias string) string {
			ids := groupedCitationIDRE.FindAllString(alias, -1)
			var replacement strings.Builder
			for _, id := range ids {
				if _, exists := known[id]; !exists {
					return alias
				}
				replacement.WriteString(canonicalCitationTag(id))
			}
			return replacement.String()
		})
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
