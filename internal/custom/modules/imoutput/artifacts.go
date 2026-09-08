package imoutput

import (
	"fmt"
	"github.com/Tencent/WeKnora/internal/types"
	"net/url"
	"strings"
)

// Artifact access remains authenticated. Only the outbound IM representation
// contains these links; canonical answer prose and browser cards stay separate.
func AppendArtifacts(answer string, files []types.MessageArtifact, origin string, dialect Dialect) string {
	base, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return answer
	}
	for _, file := range files {
		target, err := url.Parse(file.DownloadURL)
		if err != nil || target.IsAbs() || target.Host != "" || !strings.HasPrefix(target.Path, "/api/v1/custom/agent-runtime/artifacts/") {
			continue
		}
		link := base.ResolveReference(target).String()
		name := displayTitle(file.FileName, "文件")
		switch dialect {
		case DialectPlain:
			answer += "\n\n" + name + "：" + link
		case DialectSlack:
			answer += fmt.Sprintf("\n\n<%s|%s>", escapeSlackURL(link), strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "|", " ").Replace(name))
		default:
			answer += fmt.Sprintf("\n\n[%s](%s)", strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(name), escapeMarkdownURL(link))
		}
	}
	return answer
}
