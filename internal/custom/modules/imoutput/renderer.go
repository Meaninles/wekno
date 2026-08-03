// Package imoutput owns the single, deterministic final-output conversion used
// by every IM adapter. Models and stored messages keep the canonical <src>
// protocol; only the final outbound copy is rendered for the target platform.
package imoutput

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
)

type Dialect string

const (
	DialectMarkdown Dialect = "markdown"
	DialectSlack    Dialect = "slack_mrkdwn"
	DialectPlain    Dialect = "plain_text"
)

type Options struct {
	FrontendBaseURL string           `json:"frontend_base_url,omitempty"`
	Platform        string           `json:"platform,omitempty"`
	Streaming       bool             `json:"streaming"`
	TenantID        uint64           `json:"-"`
	ReferenceSigner *ReferenceSigner `json:"-"`
}

type Reference struct {
	Number     int    `json:"number"`
	CitationID string `json:"citation_id"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Target     string `json:"target"`
}

type Result struct {
	Content    string                              `json:"content"`
	References []Reference                         `json:"references"`
	Validation sourcerefs.CitationValidationReport `json:"validation"`
	Dialect    Dialect                             `json:"dialect"`
}

var (
	canonicalCitationRE = regexp.MustCompile(`<src id="(S[1-9][0-9]*)" />`)
	protectedCodeRE     = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|`[^`\\n]*`")
)

// Render validates the canonical answer against its message-bound source
// registry, then converts only valid references. It never guesses a source,
// invokes a model, retries generation, or accepts a legacy citation shape.
func Render(answer string, refs []*types.SearchResult, options Options) Result {
	filtered, citedRefs, report := sourcerefs.FilterAnswerCitations(answer, refs)
	dialect := ResolveDialect(options.Platform, options.Streaming)
	result := Result{Content: filtered, Validation: report, Dialect: dialect}

	sourcesByID := make(map[string]*sourcerefs.CitationSource)
	for _, source := range sourcerefs.SourcesFromReferences(citedRefs) {
		if source != nil && source.ID != "" {
			sourcesByID[source.ID] = source
		}
	}

	byID := make(map[string]Reference)
	for _, citationID := range report.CitedIDs {
		source := sourcesByID[citationID]
		if source == nil {
			continue
		}
		target, ok := ReferenceTarget(source, options.FrontendBaseURL, options.TenantID, options.ReferenceSigner)
		if !ok {
			continue
		}
		reference := Reference{
			Number:     len(result.References) + 1,
			CitationID: citationID,
			Type:       source.Type,
			Title:      displayTitle(source.Title, citationID),
			Target:     target,
		}
		byID[citationID] = reference
		result.References = append(result.References, reference)
	}

	result.Content = transformOutsideMarkdownCode(filtered, func(segment string) string {
		return canonicalCitationRE.ReplaceAllStringFunc(segment, func(tag string) string {
			match := canonicalCitationRE.FindStringSubmatch(tag)
			if len(match) != 2 {
				return ""
			}
			reference, ok := byID[match[1]]
			if !ok {
				// A registry-backed citation without an exact supported target is
				// omitted rather than silently downgraded to a whole document.
				return ""
			}
			return renderInlineReference(reference, dialect)
		})
	})

	return result
}

// ResolveDialect keeps platform-specific presentation decisions in this one
// module. Adapters remain transport-only. Unknown future adapters inherit the
// documented ReplyMessage Markdown contract automatically.
func ResolveDialect(platform string, streaming bool) Dialect {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "slack":
		return DialectSlack
	case "wechat", "qqbot":
		return DialectPlain
	case "feishu":
		if !streaming {
			return DialectPlain
		}
	}
	return DialectMarkdown
}

// ReferenceTarget builds an exact target for the three supported citation
// types: document fragment, Wiki page, and web page.
func ReferenceTarget(
	source *sourcerefs.CitationSource,
	frontendBaseURL string,
	tenantID uint64,
	signer *ReferenceSigner,
) (string, bool) {
	if source == nil {
		return "", false
	}
	switch source.Type {
	case sourcerefs.SourceTypeKnowledge:
		if strings.TrimSpace(source.KnowledgeBaseID) == "" ||
			strings.TrimSpace(source.KnowledgeID) == "" ||
			strings.TrimSpace(source.ChunkID) == "" {
			return "", false
		}
		token, err := signer.Issue(source, tenantID)
		if err != nil {
			return "", false
		}
		query := url.Values{"token": []string{token}}
		path := ReferenceRedirectPath + "?" + query.Encode()
		return joinFrontendURL(frontendBaseURL, path)
	case sourcerefs.SourceTypeWiki:
		if strings.TrimSpace(source.KnowledgeBaseID) == "" || strings.TrimSpace(source.Slug) == "" {
			return "", false
		}
		token, err := signer.Issue(source, tenantID)
		if err != nil {
			return "", false
		}
		query := url.Values{"token": []string{token}}
		path := ReferenceRedirectPath + "?" + query.Encode()
		return joinFrontendURL(frontendBaseURL, path)
	case sourcerefs.SourceTypeWeb:
		parsed, err := url.Parse(strings.TrimSpace(source.URL))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return "", false
		}
		return parsed.String(), true
	default:
		// Data-source retrieval statistics are not a fourth citation type.
		return "", false
	}
}

func joinFrontendURL(baseURL, path string) (string, bool) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		// External IM clients cannot resolve SPA-relative citation targets.
		// Do not emit a deceptively clickable reference when the public origin
		// has not been configured.
		return "", false
	}
	return baseURL + path, true
}

func renderInlineReference(reference Reference, dialect Dialect) string {
	switch dialect {
	case DialectSlack:
		return fmt.Sprintf("<%s|[%d]>", escapeSlackURL(reference.Target), reference.Number)
	case DialectPlain:
		return fmt.Sprintf("[%d]", reference.Number)
	default:
		// Escaped inner brackets render a clickable citation whose visible
		// label is "[1]" and cannot be mistaken for legacy [[wiki]] syntax by
		// the existing IM cleanup pipeline.
		return fmt.Sprintf("[\\[%d\\]](%s)", reference.Number, escapeMarkdownURL(reference.Target))
	}
}

func displayTitle(title, fallback string) string {
	title = strings.Join(strings.Fields(strings.TrimSpace(title)), " ")
	if title == "" {
		return fallback
	}
	return title
}

func escapeMarkdownURL(value string) string {
	return strings.NewReplacer(
		"\\", "%5C",
		" ", "%20",
		"(", "%28",
		")", "%29",
	).Replace(value)
}

func escapeSlackURL(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "%3C", ">", "%3E", "|", "%7C").Replace(value)
}

func transformOutsideMarkdownCode(content string, transform func(string) string) string {
	if content == "" || transform == nil {
		return content
	}
	indices := protectedCodeRE.FindAllStringIndex(content, -1)
	if len(indices) == 0 {
		return transform(content)
	}
	var out strings.Builder
	out.Grow(len(content))
	start := 0
	for _, index := range indices {
		out.WriteString(transform(content[start:index[0]]))
		out.WriteString(content[index[0]:index[1]])
		start = index[1]
	}
	out.WriteString(transform(content[start:]))
	return out.String()
}
