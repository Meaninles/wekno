package service

import (
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
)

// TestSanitizeManualDownloadFilename covers the filename-sanitization logic used
// by the manual-knowledge download path in GetKnowledgeFile.
func TestSanitizeManualDownloadFilename(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{
			name:  "normal title produces title.md",
			title: "My Knowledge Article",
			want:  "My Knowledge Article.md",
		},
		{
			name:  "forward slash replaced with dash",
			title: "path/to/file",
			want:  "path-to-file.md",
		},
		{
			name:  "backslash replaced with dash",
			title: `windows\path`,
			want:  "windows-path.md",
		},
		{
			name:  "double-quote replaced with single-quote",
			title: `say "hello"`,
			want:  "say 'hello'.md",
		},
		{
			name:  "newline stripped",
			title: "line1\nline2",
			want:  "line1line2.md",
		},
		{
			name:  "carriage return stripped",
			title: "line1\rline2",
			want:  "line1line2.md",
		},
		{
			name:  "combination of dangerous chars",
			title: "att\nack\r/header\\ \"injection\"",
			want:  "attack-header- 'injection'.md",
		},
		{
			name:  "blank title falls back to untitled",
			title: "",
			want:  "untitled.md",
		},
		{
			name:  "whitespace-only title falls back to untitled",
			title: "   \t  ",
			want:  "untitled.md",
		},
		{
			name:  "title that sanitizes to only whitespace falls back to untitled",
			title: "\n\r",
			want:  "untitled.md",
		},
		{
			name:  "semicolon and equals preserved (safe in quoted header value)",
			title: "a=b; c=d",
			want:  "a=b; c=d.md",
		},
		{
			name:  "Chinese title preserved",
			title: "知识库文章",
			want:  "知识库文章.md",
		},
		{
			name:  "tab character stripped",
			title: "file\tname",
			want:  "filename.md",
		},
		{
			name:  "title already ending in .md not double-extended",
			title: "guide.md",
			want:  "guide.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeManualDownloadFilename(tt.title)
			if got != tt.want {
				t.Errorf("sanitizeManualDownloadFilename(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestManualKnowledgeUpdateValuesPreservesStorageOwnership(t *testing.T) {
	knowledge := &types.Knowledge{
		ID:                   "manual-1",
		ParseStatus:          types.ParseStatusPending,
		ProcessingGeneration: "generation-2",
		ProcessingOwner:      "owner-2",
		StorageSize:          4096,
		ErrorMessage:         "",
		EnableStatus:         "disabled",
		EmbeddingModelID:     "embedding-1",
	}

	values := manualKnowledgeUpdateValues(knowledge)
	_, writesStorage := values["storage_size"]
	assert.False(t, writesStorage, "metadata/status updates must not discard or recreate a tenant storage charge")
	assert.Equal(t, "generation-2", values["processing_generation"])
	assert.Equal(t, "owner-2", values["processing_owner"])
	assert.Equal(t, types.ParseStatusPending, values["parse_status"])
}

func TestProcessingFailureValuesPreserveRecoverableStorage(t *testing.T) {
	values := processingFailureValuesPreservingStorage("cleanup failed", time.Unix(123, 0))
	_, writesStorage := values["storage_size"]
	assert.False(t, writesStorage, "failure handling must leave the row as owner of any unreleased charge")
	assert.Equal(t, types.ParseStatusFailed, values["parse_status"])
	assert.Equal(t, "cleanup failed", values["error_message"])
}

func TestCleanupArtifactsPrecedesFailureTransition(t *testing.T) {
	var order []string
	err := cleanupArtifactsBeforeFailureTransition(
		func() error {
			order = append(order, "cleanup")
			return nil
		},
		func() error {
			order = append(order, "failed")
			return nil
		},
	)
	assert.NoError(t, err)
	assert.Equal(t, []string{"cleanup", "failed"}, order)
}

func TestCleanupFailureLeavesLifecycleActive(t *testing.T) {
	cleanupErr := errors.New("object store unavailable")
	transitioned := false
	err := cleanupArtifactsBeforeFailureTransition(
		func() error { return cleanupErr },
		func() error {
			transitioned = true
			return nil
		},
	)
	assert.ErrorIs(t, err, cleanupErr)
	assert.False(t, transitioned, "Failed must not be published while partial artifacts may remain")
}
