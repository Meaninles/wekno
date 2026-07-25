package documentpreview

import (
	"path/filepath"
	"strings"
)

const (
	ModeOriginal    = "original"
	ModePagedChunks = "paged_chunks"

	MetadataModeKey   = "_system_preview_mode"
	MetadataReasonKey = "_system_preview_reason"

	MaxComplexDocumentChunks int64 = 240
)

const mebibyte int64 = 1024 * 1024

// Policy is the server-authoritative admission result for an in-browser
// original-file preview. A paged_chunks result means the UI must use already
// persisted parser chunks and must not fetch the original object into a Blob.
type Policy struct {
	Mode             string `json:"mode"`
	Reason           string `json:"reason,omitempty"`
	FileType         string `json:"file_type"`
	FileSize         int64  `json:"file_size"`
	ChunkCount       int64  `json:"chunk_count"`
	MaxOriginalBytes int64  `json:"max_original_bytes"`
}

var originalByteLimits = map[string]int64{
	"pdf":      24 * mebibyte,
	"docx":     4 * mebibyte,
	"pptx":     8 * mebibyte,
	"xlsx":     2 * mebibyte,
	"csv":      2 * mebibyte,
	"jpg":      5 * mebibyte,
	"jpeg":     5 * mebibyte,
	"png":      5 * mebibyte,
	"gif":      1 * mebibyte,
	"txt":      2 * mebibyte,
	"text":     2 * mebibyte,
	"md":       2 * mebibyte,
	"markdown": 2 * mebibyte,
	"json":     1 * mebibyte,
	"xml":      1 * mebibyte,
	"html":     1 * mebibyte,
	"css":      1 * mebibyte,
	"js":       1 * mebibyte,
	"ts":       1 * mebibyte,
	"py":       1 * mebibyte,
	"go":       1 * mebibyte,
	"java":     1 * mebibyte,
	"yaml":     1 * mebibyte,
	"yml":      1 * mebibyte,
	"sh":       1 * mebibyte,
	"ini":      1 * mebibyte,
	"conf":     1 * mebibyte,
	"log":      1 * mebibyte,
	"sql":      1 * mebibyte,
	"rs":       1 * mebibyte,
	"rb":       1 * mebibyte,
	"php":      1 * mebibyte,
	"swift":    1 * mebibyte,
	"kt":       1 * mebibyte,
	"scala":    1 * mebibyte,
	"r":        1 * mebibyte,
	"lua":      1 * mebibyte,
	"pl":       1 * mebibyte,
	"toml":     1 * mebibyte,
	"mp3":      20 * mebibyte,
	"wav":      20 * mebibyte,
	"m4a":      20 * mebibyte,
	"flac":     20 * mebibyte,
	"ogg":      20 * mebibyte,
}

var structurallyVerifiedTypes = map[string]struct{}{
	"docx": {},
	"xlsx": {},
	"pptx": {},
	"csv":  {},
}

// Types whose browser decoders cannot currently be bounded by the upload
// structural preflight are intentionally shown through parser chunks.
var alwaysPagedTypes = map[string]struct{}{
	"doc":  {},
	"xls":  {},
	"ppt":  {},
	"bmp":  {},
	"webp": {},
	"tiff": {},
	"svg":  {},
}

func NormalizeFileType(value string) string {
	value = strings.TrimSpace(strings.ToLower(strings.TrimPrefix(value, ".")))
	if value == "" {
		return ""
	}
	if ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(value)), "."); ext != "" {
		return ext
	}
	return value
}

// AnnotateUploadMetadata records the result of the upload-time structural
// preflight. Reserved keys always override caller metadata, preventing a
// request from opting a heavy archive back into an unsafe browser render.
func AnnotateUploadMetadata(
	input map[string]string,
	fileType string,
	heavy bool,
	splitRequired bool,
) map[string]string {
	result := make(map[string]string, len(input)+2)
	for key, value := range input {
		result[key] = value
	}
	normalized := NormalizeFileType(fileType)
	switch {
	case splitRequired:
		result[MetadataModeKey] = ModePagedChunks
		result[MetadataReasonKey] = "physical_split"
	case heavy:
		result[MetadataModeKey] = ModePagedChunks
		result[MetadataReasonKey] = "heavy_structure"
	default:
		if _, alwaysPaged := alwaysPagedTypes[normalized]; alwaysPaged {
			result[MetadataModeKey] = ModePagedChunks
			result[MetadataReasonKey] = "unbounded_decoder"
		} else {
			result[MetadataModeKey] = ModeOriginal
			result[MetadataReasonKey] = ""
		}
	}
	return result
}

func Decide(fileType string, fileSize, chunkCount int64, metadata map[string]string) Policy {
	normalized := NormalizeFileType(fileType)
	limit, supported := originalByteLimits[normalized]
	result := Policy{
		Mode:             ModePagedChunks,
		FileType:         normalized,
		FileSize:         fileSize,
		ChunkCount:       chunkCount,
		MaxOriginalBytes: limit,
	}
	if _, alwaysPaged := alwaysPagedTypes[normalized]; alwaysPaged {
		result.Reason = "unbounded_decoder"
		return result
	}
	if !supported {
		result.Reason = "unsupported"
		return result
	}
	if fileSize <= 0 {
		result.Reason = "unknown_size"
		return result
	}
	if fileSize > limit {
		result.Reason = "file_too_large"
		return result
	}
	if metadata != nil && metadata[MetadataModeKey] == ModePagedChunks {
		result.Reason = strings.TrimSpace(metadata[MetadataReasonKey])
		if result.Reason == "" {
			result.Reason = "upload_preflight"
		}
		return result
	}
	if _, needsVerification := structurallyVerifiedTypes[normalized]; needsVerification {
		if chunkCount < 0 {
			result.Reason = "chunk_count_unavailable"
			return result
		}
		if chunkCount > MaxComplexDocumentChunks {
			result.Reason = "too_many_chunks"
			return result
		}
		if metadata == nil || metadata[MetadataModeKey] != ModeOriginal {
			result.Reason = "unverified_structure"
			return result
		}
	}
	result.Mode = ModeOriginal
	result.Reason = ""
	return result
}

func AllowsOriginal(policy Policy) bool {
	return policy.Mode == ModeOriginal && policy.FileSize > 0 &&
		policy.MaxOriginalBytes > 0 && policy.FileSize <= policy.MaxOriginalBytes
}
