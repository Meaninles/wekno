// Package plannedfile owns the provider-independent security contract for
// crash-safe storage reservations. It never performs I/O: callers first
// reserve a canonical provider path, durably record it, and only then commit
// bytes to that exact path.
package plannedfile

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// ValidateSegment rejects anything that could change object hierarchy. It is
// intentionally stricter than filepath.Base-style sanitizing: reservations
// must reject traversal-shaped input instead of silently rewriting it.
func ValidateSegment(label, value string) error {
	if value == "" || strings.TrimSpace(value) != value || value == "." || value == ".." {
		return fmt.Errorf("planned file: invalid %s", label)
	}
	if len(value) > 255 || strings.ContainsAny(value, `/\`) || strings.Contains(value, "..") {
		return fmt.Errorf("planned file: invalid %s", label)
	}
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) {
			return fmt.Errorf("planned file: invalid %s", label)
		}
	}
	return nil
}

func ValidateOwner(tenantID uint64, knowledgeID string) error {
	if tenantID == 0 {
		return fmt.Errorf("planned file: tenant ID is required")
	}
	return ValidateSegment("knowledge ID", knowledgeID)
}

func Extension(fileName string) (string, error) {
	if err := ValidateSegment("file name", fileName); err != nil {
		return "", err
	}
	return path.Ext(fileName), nil
}

// NormalizePrefix canonicalizes a configured object prefix while rejecting
// traversal, schemes, Windows separators, and ambiguous empty components.
func NormalizePrefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", nil
	}
	if strings.Contains(prefix, `\`) || strings.Contains(prefix, "://") {
		return "", fmt.Errorf("planned file: invalid object prefix")
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	if err := ValidateKey(prefix, ""); err != nil {
		return "", fmt.Errorf("planned file: invalid object prefix: %w", err)
	}
	return prefix, nil
}

// BuildKey returns a canonical object key rooted at prefix.
func BuildKey(prefix string, segments ...string) (string, error) {
	normalized, err := NormalizePrefix(prefix)
	if err != nil {
		return "", err
	}
	for i, segment := range segments {
		if err := ValidateSegment(fmt.Sprintf("path segment %d", i), segment); err != nil {
			return "", err
		}
	}
	parts := make([]string, 0, len(segments)+1)
	if normalized != "" {
		parts = append(parts, normalized)
	}
	parts = append(parts, segments...)
	key := strings.Join(parts, "/")
	if err := ValidateKey(key, normalized); err != nil {
		return "", err
	}
	return key, nil
}

func NewObjectName(fileName string) (string, error) {
	ext, err := Extension(fileName)
	if err != nil {
		return "", err
	}
	return uuid.NewString() + ext, nil
}

func FileKey(prefix string, tenantID uint64, knowledgeID, fileName string) (string, error) {
	if err := ValidateOwner(tenantID, knowledgeID); err != nil {
		return "", err
	}
	name, err := NewObjectName(fileName)
	if err != nil {
		return "", err
	}
	return BuildKey(prefix, strconv.FormatUint(tenantID, 10), knowledgeID, name)
}

func BytesKey(prefix string, tenantID uint64, fileName string, segmentsBeforeName ...string) (string, error) {
	if tenantID == 0 {
		return "", fmt.Errorf("planned file: tenant ID is required")
	}
	name, err := NewObjectName(fileName)
	if err != nil {
		return "", err
	}
	segments := []string{strconv.FormatUint(tenantID, 10)}
	segments = append(segments, segmentsBeforeName...)
	segments = append(segments, name)
	return BuildKey(prefix, segments...)
}

// ValidateKey proves key is canonical and, when prefix is configured, cannot
// escape that exact prefix namespace.
func ValidateKey(key, prefix string) error {
	if key == "" || strings.TrimSpace(key) != key || strings.HasPrefix(key, "/") ||
		strings.HasSuffix(key, "/") || strings.Contains(key, `\`) || strings.Contains(key, "?") ||
		strings.Contains(key, "#") || len(key) > 2048 {
		return fmt.Errorf("planned file: invalid object key")
	}
	for _, segment := range strings.Split(key, "/") {
		if err := ValidateSegment("object-key segment", segment); err != nil {
			return err
		}
	}
	normalized, err := NormalizePrefixForValidation(prefix)
	if err != nil {
		return err
	}
	if normalized != "" && !strings.HasPrefix(key, normalized+"/") {
		return fmt.Errorf("planned file: object key escapes configured prefix")
	}
	return nil
}

// NormalizePrefixForValidation avoids recursion through ValidateKey.
func NormalizePrefixForValidation(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", nil
	}
	if strings.Contains(prefix, `\`) || strings.Contains(prefix, "://") {
		return "", fmt.Errorf("planned file: invalid object prefix")
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	for _, segment := range strings.Split(prefix, "/") {
		if err := ValidateSegment("object-prefix segment", segment); err != nil {
			return "", err
		}
	}
	return prefix, nil
}

func FormatBucketPath(scheme, bucket, key string) (string, error) {
	if err := ValidateSegment("provider", scheme); err != nil {
		return "", err
	}
	if err := ValidateSegment("bucket", bucket); err != nil {
		return "", err
	}
	if err := ValidateKey(key, ""); err != nil {
		return "", err
	}
	return scheme + "://" + bucket + "/" + key, nil
}

func ParseBucketPath(filePath, scheme, bucket, requiredPrefix string) (string, error) {
	if err := ValidateSegment("provider", scheme); err != nil {
		return "", err
	}
	if err := ValidateSegment("bucket", bucket); err != nil {
		return "", err
	}
	prefix := scheme + "://"
	if !strings.HasPrefix(filePath, prefix) {
		return "", fmt.Errorf("planned file: path provider mismatch")
	}
	rest := strings.TrimPrefix(filePath, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] != bucket {
		return "", fmt.Errorf("planned file: path bucket mismatch")
	}
	if err := ValidateKey(parts[1], requiredPrefix); err != nil {
		return "", err
	}
	return parts[1], nil
}

func FormatRegionPath(scheme, bucket, region, key string) (string, error) {
	if err := ValidateSegment("region", region); err != nil {
		return "", err
	}
	base, err := FormatBucketPath(scheme, bucket, region+"/"+key)
	if err != nil {
		return "", err
	}
	return base, nil
}

func ParseRegionPath(filePath, scheme, bucket, region, requiredPrefix string) (string, error) {
	if err := ValidateSegment("region", region); err != nil {
		return "", err
	}
	key, err := ParseBucketPath(filePath, scheme, bucket, "")
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 || parts[0] != region {
		return "", fmt.Errorf("planned file: path region mismatch")
	}
	if err := ValidateKey(parts[1], requiredPrefix); err != nil {
		return "", err
	}
	return parts[1], nil
}
