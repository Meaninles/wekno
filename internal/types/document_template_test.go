package types

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestDefaultDocumentTemplateConfigIncludesPPT(t *testing.T) {
	cfg := DefaultDocumentTemplateConfig()
	if cfg == nil {
		t.Fatalf("default config is nil")
	}
	if cfg.PPT.RequirementFile == nil {
		t.Fatalf("default config should include PPT template requirement")
	}
	if cfg.PPT.RequirementFile.BuiltinID != BuiltinDocumentTemplateRequirementPPT {
		t.Fatalf("PPT builtin requirement = %q, want %q", cfg.PPT.RequirementFile.BuiltinID, BuiltinDocumentTemplateRequirementPPT)
	}
}

func TestNormalizeDocumentTemplateConfigAcceptsPPTReferences(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("ppt bytes"))
	cfg := &DocumentTemplateConfig{
		PPT: DocumentTemplateFormatConfig{
			RequirementFile: &DocumentTemplateFile{
				Source:        DocumentTemplateFileSourceUpload,
				FileName:      "ppt-rules.md",
				FileType:      "md",
				ContentBase64: base64.StdEncoding.EncodeToString([]byte("# rules")),
			},
			TemplateFiles: []DocumentTemplateFile{
				{Source: DocumentTemplateFileSourceUpload, FileName: "old-template.ppt", ContentBase64: payload},
				{Source: DocumentTemplateFileSourceUpload, FileName: "new-template.pptx", ContentBase64: payload},
				{Source: DocumentTemplateFileSourceUpload, FileName: "alt-template.pptx", ContentBase64: payload},
			},
		},
	}

	if err := normalizeDocumentTemplateConfig(cfg); err != nil {
		t.Fatalf("normalizeDocumentTemplateConfig returned error: %v", err)
	}
	if cfg.PPT.RequirementFile == nil || cfg.PPT.RequirementFile.FileType != "md" {
		t.Fatalf("PPT requirement not normalized: %+v", cfg.PPT.RequirementFile)
	}
	if len(cfg.PPT.TemplateFiles) != 3 {
		t.Fatalf("PPT template file count = %d, want 3", len(cfg.PPT.TemplateFiles))
	}
	if cfg.PPT.TemplateFiles[0].FileType != "ppt" || cfg.PPT.TemplateFiles[1].FileType != "pptx" {
		t.Fatalf("PPT reference file types not normalized: %+v", cfg.PPT.TemplateFiles)
	}
}

func TestNormalizeDocumentTemplateConfigRejectsFourPPTReferences(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("ppt bytes"))
	files := make([]DocumentTemplateFile, 0, 4)
	for i := 0; i < 4; i++ {
		files = append(files, DocumentTemplateFile{
			Source:        DocumentTemplateFileSourceUpload,
			FileName:      "template.pptx",
			ContentBase64: payload,
		})
	}
	cfg := &DocumentTemplateConfig{
		PPT: DocumentTemplateFormatConfig{TemplateFiles: files},
	}

	err := normalizeDocumentTemplateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "PPT 参考文档最多只能上传 3 份") {
		t.Fatalf("normalizeDocumentTemplateConfig error = %v, want PPT reference limit error", err)
	}
}

func TestNormalizeDocumentTemplateConfigKeepsWordReferenceLimitAtThree(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("word bytes"))
	files := make([]DocumentTemplateFile, 0, 4)
	for i := 0; i < 4; i++ {
		files = append(files, DocumentTemplateFile{
			Source:        DocumentTemplateFileSourceUpload,
			FileName:      "template.docx",
			ContentBase64: payload,
		})
	}
	cfg := &DocumentTemplateConfig{
		Word: DocumentTemplateFormatConfig{TemplateFiles: files},
	}

	err := normalizeDocumentTemplateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "Word 参考文档最多只能上传 3 份") {
		t.Fatalf("normalizeDocumentTemplateConfig error = %v, want Word reference limit error", err)
	}
}

func TestNormalizeDocumentTemplateConfigAllowsPPTReferenceAboveDefaultFileSizeLimit(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString(make([]byte, documentTemplateDefaultMaxFileSizeBytes+1))
	cfg := &DocumentTemplateConfig{
		PPT: DocumentTemplateFormatConfig{
			TemplateFiles: []DocumentTemplateFile{
				{Source: DocumentTemplateFileSourceUpload, FileName: "template.pptx", ContentBase64: payload},
			},
		},
	}

	if err := normalizeDocumentTemplateConfig(cfg); err != nil {
		t.Fatalf("normalizeDocumentTemplateConfig returned error: %v", err)
	}
}

func TestDocumentTemplateFileSizeLimitKeepsPPTRequirementAtDefault(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString(make([]byte, documentTemplateDefaultMaxFileSizeBytes+1))
	cfg := &DocumentTemplateConfig{
		PPT: DocumentTemplateFormatConfig{
			RequirementFile: &DocumentTemplateFile{
				Source:        DocumentTemplateFileSourceUpload,
				FileName:      "ppt-rules.md",
				ContentBase64: payload,
			},
		},
	}

	err := normalizeDocumentTemplateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "PPT 模板要求文件 文件不能超过 20 MB") {
		t.Fatalf("normalizeDocumentTemplateConfig error = %v, want PPT requirement size limit error", err)
	}
}

func TestNormalizeDocumentTemplateConfigRejectsWordReferenceAboveDefaultFileSizeLimit(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString(make([]byte, documentTemplateDefaultMaxFileSizeBytes+1))
	cfg := &DocumentTemplateConfig{
		Word: DocumentTemplateFormatConfig{
			TemplateFiles: []DocumentTemplateFile{
				{Source: DocumentTemplateFileSourceUpload, FileName: "template.docx", ContentBase64: payload},
			},
		},
	}

	err := normalizeDocumentTemplateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "Word 参考文档1 文件不能超过 20 MB") {
		t.Fatalf("normalizeDocumentTemplateConfig error = %v, want Word file size limit error", err)
	}
}

func TestNormalizeDocumentTemplateConfigRejectsUnsupportedPPTReference(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("pdf bytes"))
	cfg := &DocumentTemplateConfig{
		PPT: DocumentTemplateFormatConfig{
			TemplateFiles: []DocumentTemplateFile{
				{Source: DocumentTemplateFileSourceUpload, FileName: "template.pdf", ContentBase64: payload},
			},
		},
	}

	err := normalizeDocumentTemplateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "PPT 参考文档1 文件类型不支持: pdf") {
		t.Fatalf("normalizeDocumentTemplateConfig error = %v, want PPT extension error", err)
	}
}
