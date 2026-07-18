package generalagent

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestResolveRunFileRejectsSelectedKnowledgeAsIngestionInput(t *testing.T) {
	run := &activeRun{
		runID: "run-selected-doc",
		originalInputFiles: map[string]OriginalInputFileSpec{
			"selected": {
				ID:         "selected",
				Source:     types.OriginalInputSourceSelectedKnowledge,
				StorageURL: "trusted/object",
				FileName:   "old.pdf",
				FileSize:   1,
				SHA256:     strings.Repeat("0", 64),
			},
		},
	}
	unregister := registerActiveRun(run)
	defer unregister()

	_, _, _, err := (&Service{}).ResolveRunFile(context.Background(), run.runID, "input_file", "selected")
	if err == nil || !strings.Contains(err.Error(), "current-turn uploaded attachment") {
		t.Fatalf("ResolveRunFile() error = %v, want selected-knowledge rejection", err)
	}
}

func TestResolveRunFileRejectsURLAndPathSourceKinds(t *testing.T) {
	run := &activeRun{runID: "run-source-kind", originalInputFiles: map[string]OriginalInputFileSpec{}}
	unregister := registerActiveRun(run)
	defer unregister()

	for _, sourceType := range []string{"url", "path", "file", "https"} {
		_, _, _, err := (&Service{}).ResolveRunFile(context.Background(), run.runID, sourceType, "https://example.test/file.pdf")
		if err == nil || !strings.Contains(err.Error(), "artifact or input_file") {
			t.Fatalf("source type %q error = %v", sourceType, err)
		}
	}
}
