package skillhub

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const maxProfessionalSkillPathRunes = 240

func normalizeProfessionalSkillRelativePath(raw string) (string, error) {
	rel := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	rel = strings.TrimPrefix(rel, "./")
	rel = norm.NFC.String(rel)
	if rel == "" || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("path must be relative")
	}
	if !utf8.ValidString(rel) {
		return "", fmt.Errorf("path must be valid UTF-8")
	}
	if len([]rune(rel)) > maxProfessionalSkillPathRunes {
		return "", fmt.Errorf("path exceeds %d characters", maxProfessionalSkillPathRunes)
	}
	for _, r := range rel {
		if r == 0 || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return "", fmt.Errorf("path contains unsafe characters")
		}
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
			return "", fmt.Errorf("path contains unsafe segment")
		}
	}
	clean := path.Clean(rel)
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("path escapes skill directory")
	}
	if len([]rune(clean)) > maxProfessionalSkillPathRunes {
		return "", fmt.Errorf("path exceeds %d characters", maxProfessionalSkillPathRunes)
	}
	return clean, nil
}
