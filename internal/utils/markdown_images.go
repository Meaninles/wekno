package utils

import "regexp"

var linkedMarkdownImage = regexp.MustCompile(`\[!\[([^\]]*)\]\(([^()\s]*(?:\([^)]*\)[^()\s]*)*)\)\]\([^()\s]*(?:\([^)]*\)[^()\s]*)*\)`)

// UnwrapLinkedImages removes only an image's surrounding Markdown hyperlink.
// Pure text handling belongs below parsers and model adapters in the dependency
// graph; splitting must not import the networked parser/service stack.
func UnwrapLinkedImages(markdown string) string {
	return linkedMarkdownImage.ReplaceAllString(markdown, "![$1]($2)")
}
