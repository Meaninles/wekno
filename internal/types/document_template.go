package types

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	DocumentFormatWord  = "word"
	DocumentFormatExcel = "excel"
	DocumentFormatPDF   = "pdf"
	DocumentFormatPPT   = "ppt"

	DocumentTemplateFileSourceBuiltin = "builtin"
	DocumentTemplateFileSourceUpload  = "upload"

	BuiltinDocumentTemplateRequirementWord  = "gbt9704_2012_word_requirement"
	BuiltinDocumentTemplateRequirementExcel = "gbt9704_2012_excel_requirement"
	BuiltinDocumentTemplateRequirementPDF   = "gbt9704_2012_pdf_requirement"
	BuiltinDocumentTemplateRequirementPPT   = "gbt9704_2012_ppt_requirement"
)

const (
	documentTemplateDefaultReferenceLimit = 3
	documentTemplatePPTReferenceLimit     = 3

	documentTemplateDefaultMaxFileSizeBytes      int64 = 20 * 1024 * 1024
	documentTemplatePPTReferenceMaxFileSizeBytes int64 = 100 * 1024 * 1024
)

// DocumentTemplateConfig stores document-processing template requirements and
// soft reference templates. It is intentionally scoped to the Claude SDK
// document-processing agent type.
type DocumentTemplateConfig struct {
	Word  DocumentTemplateFormatConfig `yaml:"word" json:"word"`
	Excel DocumentTemplateFormatConfig `yaml:"excel" json:"excel"`
	PDF   DocumentTemplateFormatConfig `yaml:"pdf" json:"pdf"`
	PPT   DocumentTemplateFormatConfig `yaml:"ppt" json:"ppt"`
}

type DocumentTemplateFormatConfig struct {
	RequirementFile *DocumentTemplateFile  `yaml:"requirement_file" json:"requirement_file,omitempty"`
	TemplateFiles   []DocumentTemplateFile `yaml:"template_files" json:"template_files,omitempty"`
}

type DocumentTemplateFile struct {
	ID            string `yaml:"id" json:"id,omitempty"`
	Source        string `yaml:"source" json:"source,omitempty"`
	BuiltinID     string `yaml:"builtin_id" json:"builtin_id,omitempty"`
	FileName      string `yaml:"file_name" json:"file_name"`
	FileType      string `yaml:"file_type" json:"file_type"`
	FileSize      int64  `yaml:"file_size" json:"file_size,omitempty"`
	ContentBase64 string `yaml:"content_base64" json:"content_base64,omitempty"`
}

type BuiltinDocumentTemplateFileInfo struct {
	ID           string
	Format       string
	FileName     string
	FileType     string
	RelativePath string
}

var builtinDocumentTemplateFiles = map[string]BuiltinDocumentTemplateFileInfo{
	BuiltinDocumentTemplateRequirementWord: {
		ID:           BuiltinDocumentTemplateRequirementWord,
		Format:       DocumentFormatWord,
		FileName:     "GB_T_9704_2012_党政机关公文格式_Word模板要求.md",
		FileType:     "md",
		RelativePath: filepath.Join("custom", "document-templates", "gbt-9704-2012", "word.md"),
	},
	BuiltinDocumentTemplateRequirementExcel: {
		ID:           BuiltinDocumentTemplateRequirementExcel,
		Format:       DocumentFormatExcel,
		FileName:     "GB_T_9704_2012_党政机关公文格式_Excel模板要求.md",
		FileType:     "md",
		RelativePath: filepath.Join("custom", "document-templates", "gbt-9704-2012", "excel.md"),
	},
	BuiltinDocumentTemplateRequirementPDF: {
		ID:           BuiltinDocumentTemplateRequirementPDF,
		Format:       DocumentFormatPDF,
		FileName:     "GB_T_9704_2012_党政机关公文格式_PDF模板要求.md",
		FileType:     "md",
		RelativePath: filepath.Join("custom", "document-templates", "gbt-9704-2012", "pdf.md"),
	},
	BuiltinDocumentTemplateRequirementPPT: {
		ID:           BuiltinDocumentTemplateRequirementPPT,
		Format:       DocumentFormatPPT,
		FileName:     "GB_T_9704_2012_党政机关公文格式_PPT模板要求.md",
		FileType:     "md",
		RelativePath: filepath.Join("custom", "document-templates", "gbt-9704-2012", "ppt.md"),
	},
}

func BuiltinDocumentTemplateFileInfoByID(id string) (BuiltinDocumentTemplateFileInfo, bool) {
	info, ok := builtinDocumentTemplateFiles[id]
	return info, ok
}

func DefaultDocumentTemplateConfig() *DocumentTemplateConfig {
	return &DocumentTemplateConfig{
		Word: DocumentTemplateFormatConfig{
			RequirementFile: builtinRequirementFile(BuiltinDocumentTemplateRequirementWord),
		},
		Excel: DocumentTemplateFormatConfig{
			RequirementFile: builtinRequirementFile(BuiltinDocumentTemplateRequirementExcel),
		},
		PDF: DocumentTemplateFormatConfig{
			RequirementFile: builtinRequirementFile(BuiltinDocumentTemplateRequirementPDF),
		},
		PPT: DocumentTemplateFormatConfig{
			RequirementFile: builtinRequirementFile(BuiltinDocumentTemplateRequirementPPT),
		},
	}
}

func builtinRequirementFile(id string) *DocumentTemplateFile {
	info, ok := BuiltinDocumentTemplateFileInfoByID(id)
	if !ok {
		return nil
	}
	return &DocumentTemplateFile{
		ID:        info.ID,
		Source:    DocumentTemplateFileSourceBuiltin,
		BuiltinID: info.ID,
		FileName:  info.FileName,
		FileType:  info.FileType,
	}
}

func NormalizeCustomAgentDocumentTemplateConfig(config *CustomAgentConfig) error {
	if config == nil {
		return nil
	}
	if config.AgentType != AgentTypeDocumentProcessingAgent {
		config.DocumentTemplate = nil
		return nil
	}
	if config.DocumentTemplate == nil {
		config.DocumentTemplate = DefaultDocumentTemplateConfig()
		return nil
	}
	return normalizeDocumentTemplateConfig(config.DocumentTemplate)
}

func normalizeDocumentTemplateConfig(config *DocumentTemplateConfig) error {
	if config == nil {
		return nil
	}
	formats := []struct {
		name string
		cfg  *DocumentTemplateFormatConfig
	}{
		{DocumentFormatWord, &config.Word},
		{DocumentFormatExcel, &config.Excel},
		{DocumentFormatPDF, &config.PDF},
		{DocumentFormatPPT, &config.PPT},
	}
	for _, item := range formats {
		if err := normalizeTemplateFormat(item.name, item.cfg); err != nil {
			return err
		}
	}
	return nil
}

func normalizeTemplateFormat(format string, cfg *DocumentTemplateFormatConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.RequirementFile != nil {
		normalized, err := normalizeTemplateFile(format, "模板要求文件", *cfg.RequirementFile, true)
		if err != nil {
			return err
		}
		cfg.RequirementFile = &normalized
	}
	referenceLimit := documentTemplateReferenceLimit(format)
	if len(cfg.TemplateFiles) > referenceLimit {
		return fmt.Errorf("%s 参考文档最多只能上传 %d 份", formatDisplayName(format), referenceLimit)
	}
	out := make([]DocumentTemplateFile, 0, len(cfg.TemplateFiles))
	for i, file := range cfg.TemplateFiles {
		normalized, err := normalizeTemplateFile(format, fmt.Sprintf("参考文档%d", i+1), file, false)
		if err != nil {
			return err
		}
		out = append(out, normalized)
	}
	cfg.TemplateFiles = out
	return nil
}

func documentTemplateReferenceLimit(format string) int {
	if format == DocumentFormatPPT {
		return documentTemplatePPTReferenceLimit
	}
	return documentTemplateDefaultReferenceLimit
}

func documentTemplateFileSizeLimit(format string, requirement bool) int64 {
	if format == DocumentFormatPPT && !requirement {
		return documentTemplatePPTReferenceMaxFileSizeBytes
	}
	return documentTemplateDefaultMaxFileSizeBytes
}

func normalizeTemplateFile(format string, role string, file DocumentTemplateFile, requirement bool) (DocumentTemplateFile, error) {
	file.Source = strings.TrimSpace(file.Source)
	file.BuiltinID = strings.TrimSpace(file.BuiltinID)
	file.FileName = strings.TrimSpace(file.FileName)
	file.FileType = normalizeFileExt(firstNonEmpty(file.FileType, filepath.Ext(file.FileName)))
	file.ContentBase64 = strings.TrimSpace(file.ContentBase64)
	if file.Source == "" {
		if file.BuiltinID != "" {
			file.Source = DocumentTemplateFileSourceBuiltin
		} else {
			file.Source = DocumentTemplateFileSourceUpload
		}
	}

	if file.Source == DocumentTemplateFileSourceBuiltin {
		info, ok := BuiltinDocumentTemplateFileInfoByID(file.BuiltinID)
		if !ok {
			return file, fmt.Errorf("%s %s 引用了未知的内置模板文件", formatDisplayName(format), role)
		}
		if info.Format != format {
			return file, fmt.Errorf("%s %s 不能引用 %s 的内置模板文件", formatDisplayName(format), role, formatDisplayName(info.Format))
		}
		file.ID = firstNonEmpty(file.ID, info.ID)
		file.FileName = info.FileName
		file.FileType = info.FileType
		file.ContentBase64 = ""
		file.FileSize = 0
		return file, nil
	}

	if file.Source != DocumentTemplateFileSourceUpload {
		return file, fmt.Errorf("%s %s 来源类型无效", formatDisplayName(format), role)
	}
	if file.FileName == "" {
		return file, fmt.Errorf("%s %s 缺少文件名", formatDisplayName(format), role)
	}
	if !allowedDocumentTemplateExt(format, file.FileType, requirement) {
		return file, fmt.Errorf("%s %s 文件类型不支持: %s", formatDisplayName(format), role, file.FileType)
	}
	if file.ContentBase64 == "" {
		return file, fmt.Errorf("%s %s 缺少原文件内容", formatDisplayName(format), role)
	}
	contentBase64 := stripDataURI(file.ContentBase64)
	data, err := base64.StdEncoding.DecodeString(contentBase64)
	if err != nil {
		return file, fmt.Errorf("%s %s 文件内容不是有效 base64", formatDisplayName(format), role)
	}
	sizeLimit := documentTemplateFileSizeLimit(format, requirement)
	if int64(len(data)) > sizeLimit {
		return file, fmt.Errorf("%s %s 文件不能超过 %s", formatDisplayName(format), role, documentTemplateFormatSize(sizeLimit))
	}
	file.ContentBase64 = contentBase64
	file.FileSize = int64(len(data))
	return file, nil
}

func documentTemplateFormatSize(bytes int64) string {
	if bytes > 0 && bytes%(1024*1024) == 0 {
		return fmt.Sprintf("%d MB", bytes/(1024*1024))
	}
	return fmt.Sprintf("%d bytes", bytes)
}

func allowedDocumentTemplateExt(format string, ext string, requirement bool) bool {
	ext = normalizeFileExt(ext)
	if requirement {
		return ext == "md" || ext == "markdown" || ext == "txt"
	}
	switch format {
	case DocumentFormatWord:
		return ext == "doc" || ext == "docx"
	case DocumentFormatExcel:
		return ext == "xls" || ext == "xlsx"
	case DocumentFormatPDF:
		return ext == "pdf"
	case DocumentFormatPPT:
		return ext == "ppt" || ext == "pptx"
	default:
		return false
	}
}

func normalizeFileExt(ext string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
}

func stripDataURI(value string) string {
	if idx := strings.Index(value, ","); idx >= 0 && strings.Contains(value[:idx], "base64") {
		return strings.TrimSpace(value[idx+1:])
	}
	return value
}

func formatDisplayName(format string) string {
	switch format {
	case DocumentFormatWord:
		return "Word"
	case DocumentFormatExcel:
		return "Excel"
	case DocumentFormatPDF:
		return "PDF"
	case DocumentFormatPPT:
		return "PPT"
	default:
		return format
	}
}

func firstNonEmpty(items ...string) string {
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			return item
		}
	}
	return ""
}
