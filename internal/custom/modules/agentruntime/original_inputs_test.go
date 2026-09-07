package agentruntime

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestOriginalInputFileSpecsIgnoresKnowledgeBaseIDs(t *testing.T) {
	svc := &Service{}
	req := &types.QARequest{
		OriginalInputFiles: []types.OriginalInputFile{
			{
				ID:          "upload-1",
				Source:      types.OriginalInputSourceChatUpload,
				Role:        types.OriginalInputRoleUserUploadedOriginal,
				FileName:    "uploaded.docx",
				FileType:    "docx",
				FileSize:    123,
				SHA256:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				DownloadURL: "http://example.invalid/uploaded.docx",
				StorageURL:  "minio://bucket/object",
			},
		},
		KnowledgeBaseIDs: []string{"kb-selected-as-whole"},
	}

	got, err := svc.originalInputFileSpecs(context.Background(), req, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("originalInputFileSpecs length = %d, want only the runtime upload", len(got))
	}
	if got[0].FileName != "uploaded.docx" {
		t.Fatalf("file name = %q, want uploaded.docx", got[0].FileName)
	}
	if got[0].Source != types.OriginalInputSourceChatUpload {
		t.Fatalf("source = %q, want %q", got[0].Source, types.OriginalInputSourceChatUpload)
	}
}

func TestOriginalInputFileSpecsSelectedKnowledgeIDRequiresKnowledgeService(t *testing.T) {
	svc := &Service{}
	req := &types.QARequest{
		KnowledgeBaseIDs: []string{"kb-selected-as-whole"},
		KnowledgeIDs:     []string{"knowledge-file-selected"},
	}

	got, err := svc.originalInputFileSpecs(context.Background(), req, "run-1")
	if err == nil || len(got) != 0 {
		t.Fatalf("unavailable selected source must fail explicitly, got files=%v err=%v", got, err)
	}
}
