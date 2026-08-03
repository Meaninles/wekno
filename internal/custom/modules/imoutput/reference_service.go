package imoutput

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"gorm.io/gorm"
)

type referenceKnowledgeFiles interface {
	GetKnowledgeFile(ctx context.Context, id string) (io.ReadCloser, string, error)
}

type PublicReferenceView struct {
	Type     string                   `json:"type"`
	Title    string                   `json:"title"`
	Document *PublicReferenceDocument `json:"document,omitempty"`
	Fragment *PublicReferenceFragment `json:"fragment,omitempty"`
	Wiki     *PublicReferenceWikiPage `json:"wiki,omitempty"`
}

type PublicReferenceDocument struct {
	Title     string    `json:"title"`
	FileName  string    `json:"file_name"`
	FileType  string    `json:"file_type"`
	FileSize  int64     `json:"file_size"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PublicReferenceFragment struct {
	Content       string          `json:"content"`
	ChunkIndex    int             `json:"chunk_index"`
	StartAt       int             `json:"start_at"`
	EndAt         int             `json:"end_at"`
	ChunkType     string          `json:"chunk_type"`
	SourceLocator json.RawMessage `json:"source_locator,omitempty"`
}

type PublicReferenceWikiPage struct {
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	PageType  string    `json:"page_type"`
	Summary   string    `json:"summary"`
	Content   string    `json:"content"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

type OriginalReferenceFile struct {
	Reader   io.ReadCloser
	FileName string
	FileType string
	FileSize int64
}

type ReferenceService struct {
	db            *gorm.DB
	files         referenceKnowledgeFiles
	signer        *ReferenceSigner
	publicBaseURL string
}

func NewReferenceService(
	db *gorm.DB,
	files referenceKnowledgeFiles,
	signer *ReferenceSigner,
	publicBaseURL ...string,
) *ReferenceService {
	baseURL := ""
	if len(publicBaseURL) > 0 {
		baseURL = strings.TrimSpace(publicBaseURL[0])
	}
	return &ReferenceService{db: db, files: files, signer: signer, publicBaseURL: baseURL}
}

func (s *ReferenceService) VerifyToken(token string) (ReferenceCapability, error) {
	if s == nil || s.signer == nil {
		return ReferenceCapability{}, ErrReferenceSigningKeyUnavailable
	}
	return s.signer.Verify(token)
}

func (s *ReferenceService) Resolve(ctx context.Context, token string) (*PublicReferenceView, error) {
	capability, err := s.VerifyToken(token)
	if err != nil {
		return nil, err
	}
	if s.db == nil {
		return nil, fmt.Errorf("IM reference database is unavailable")
	}
	switch capability.Type {
	case sourcerefs.SourceTypeKnowledge:
		row, err := s.resolveDocument(ctx, capability)
		if err != nil {
			return nil, err
		}
		title := firstReferenceValue(row.Title, row.FileName, "文档片段")
		fileName := firstReferenceValue(row.FileName, row.Title, "文档")
		fileType := firstReferenceValue(strings.TrimPrefix(row.FileType, "."), strings.TrimPrefix(filepath.Ext(fileName), "."))
		locator := json.RawMessage(nil)
		if len(row.SourceLocator) > 0 && json.Valid(row.SourceLocator) {
			locator = append(json.RawMessage(nil), row.SourceLocator...)
		}
		view := &PublicReferenceView{
			Type:  sourcerefs.SourceTypeKnowledge,
			Title: title,
			Document: &PublicReferenceDocument{
				Title: title, FileName: fileName, FileType: fileType, FileSize: row.FileSize,
				CreatedAt: row.KnowledgeCreatedAt, UpdatedAt: row.KnowledgeUpdatedAt,
			},
			Fragment: &PublicReferenceFragment{
				Content: row.Content, ChunkIndex: row.ChunkIndex, StartAt: row.StartAt,
				EndAt: row.EndAt, ChunkType: row.ChunkType, SourceLocator: locator,
			},
		}
		s.rewriteProtectedAssets(view, capability.TenantID)
		return view, nil
	case sourcerefs.SourceTypeWiki:
		row, err := s.resolveWiki(ctx, capability)
		if err != nil {
			return nil, err
		}
		view := &PublicReferenceView{
			Type:  sourcerefs.SourceTypeWiki,
			Title: firstReferenceValue(row.Title, row.Slug, "Wiki"),
			Wiki: &PublicReferenceWikiPage{
				Slug: row.Slug, Title: row.Title, PageType: row.PageType, Summary: row.Summary,
				Content: row.Content, Version: row.Version, UpdatedAt: row.UpdatedAt,
			},
		}
		s.rewriteProtectedAssets(view, capability.TenantID)
		return view, nil
	default:
		return nil, ErrInvalidReferenceCapability
	}
}

func (s *ReferenceService) OpenOriginal(ctx context.Context, token string) (*OriginalReferenceFile, error) {
	file, capability, err := s.describeOriginal(ctx, token)
	if err != nil {
		return nil, err
	}
	ownerCtx := context.WithValue(ctx, types.TenantIDContextKey, capability.TenantID)
	reader, fileName, err := s.files.GetKnowledgeFile(ownerCtx, capability.KnowledgeID)
	if err != nil {
		return nil, err
	}
	file.Reader = reader
	file.FileName = firstReferenceValue(strings.TrimSpace(fileName), file.FileName, "document")
	if file.FileType == "" {
		file.FileType = strings.TrimPrefix(filepath.Ext(file.FileName), ".")
	}
	return file, nil
}

func (s *ReferenceService) DescribeOriginal(ctx context.Context, token string) (*OriginalReferenceFile, error) {
	file, _, err := s.describeOriginal(ctx, token)
	return file, err
}

func (s *ReferenceService) describeOriginal(
	ctx context.Context,
	token string,
) (*OriginalReferenceFile, ReferenceCapability, error) {
	capability, err := s.VerifyToken(token)
	if err != nil {
		return nil, ReferenceCapability{}, err
	}
	if capability.Type != sourcerefs.SourceTypeKnowledge || s.files == nil {
		return nil, ReferenceCapability{}, ErrInvalidReferenceCapability
	}
	row, err := s.resolveDocument(ctx, capability)
	if err != nil {
		return nil, ReferenceCapability{}, err
	}
	fileName := firstReferenceValue(row.FileName, row.Title, "document")
	fileType := firstReferenceValue(strings.TrimPrefix(row.FileType, "."), strings.TrimPrefix(filepath.Ext(fileName), "."))
	return &OriginalReferenceFile{FileName: fileName, FileType: fileType, FileSize: row.FileSize}, capability, nil
}

type documentReferenceRow struct {
	Content            string
	ChunkIndex         int
	StartAt            int
	EndAt              int
	ChunkType          string
	SourceLocator      types.JSON
	Title              string
	FileName           string
	FileType           string
	FileSize           int64
	KnowledgeCreatedAt time.Time
	KnowledgeUpdatedAt time.Time
}

func (s *ReferenceService) resolveDocument(ctx context.Context, capability ReferenceCapability) (documentReferenceRow, error) {
	var row documentReferenceRow
	err := s.db.WithContext(ctx).
		Table("chunks AS c").
		Select(`c.content AS content, c.chunk_index AS chunk_index, c.start_at AS start_at,
			c.end_at AS end_at, c.chunk_type AS chunk_type, c.source_locator AS source_locator,
			k.title AS title, k.file_name AS file_name, k.file_type AS file_type,
			k.file_size AS file_size, k.created_at AS knowledge_created_at,
			k.updated_at AS knowledge_updated_at`).
		Joins("JOIN knowledges AS k ON k.id = c.knowledge_id AND k.tenant_id = c.tenant_id AND k.knowledge_base_id = c.knowledge_base_id").
		Joins("JOIN knowledge_bases AS kb ON kb.id = k.knowledge_base_id AND kb.tenant_id = k.tenant_id").
		Where("c.id = ? AND c.tenant_id = ? AND c.knowledge_base_id = ? AND c.knowledge_id = ?",
			capability.ChunkID, capability.TenantID, capability.KnowledgeBaseID, capability.KnowledgeID).
		Where("c.deleted_at IS NULL AND k.deleted_at IS NULL AND kb.deleted_at IS NULL").
		Where("c.is_enabled = ? AND c.status IN ? AND k.enable_status = ?", true,
			[]int{int(types.ChunkStatusDefault), int(types.ChunkStatusIndexed)}, "enabled").
		Take(&row).Error
	return row, err
}

type wikiReferenceRow struct {
	Slug      string
	Title     string
	PageType  string
	Summary   string
	Content   string
	Version   int
	UpdatedAt time.Time
}

func (s *ReferenceService) resolveWiki(ctx context.Context, capability ReferenceCapability) (wikiReferenceRow, error) {
	var row wikiReferenceRow
	err := s.db.WithContext(ctx).
		Table("wiki_pages AS w").
		Select("w.slug, w.title, w.page_type, w.summary, w.content, w.version, w.updated_at").
		Joins("JOIN knowledge_bases AS kb ON kb.id = w.knowledge_base_id AND kb.tenant_id = w.tenant_id").
		Where("w.tenant_id = ? AND w.knowledge_base_id = ? AND w.slug = ?",
			capability.TenantID, capability.KnowledgeBaseID, capability.Slug).
		Where("w.deleted_at IS NULL AND kb.deleted_at IS NULL AND w.status = ?", types.WikiPageStatusPublished).
		Take(&row).Error
	return row, err
}

func firstReferenceValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

var publicReferenceProviderURLRE = regexp.MustCompile(`(?i)(?:local|minio|cos|tos|s3|oss|ks3|obs)://[^\s<>"'` + "`" + `()\[\]]+`)

// rewriteProtectedAssets grants short-lived access only to provider objects
// literally present in the already-authorized fragment or Wiki page. The IM
// capability itself remains durable; re-opening the page refreshes image URLs.
// This work happens on click, never on the answer-generation path.
func (s *ReferenceService) rewriteProtectedAssets(view *PublicReferenceView, tenantID uint64) {
	if s == nil || view == nil || tenantID == 0 || s.publicBaseURL == "" {
		return
	}
	rewrite := func(content string) string {
		return publicReferenceProviderURLRE.ReplaceAllStringFunc(content, func(filePath string) string {
			signed, err := utils.SignFileURL(s.publicBaseURL, filePath, tenantID, 0)
			if err != nil {
				return filePath
			}
			return signed
		})
	}
	if view.Fragment != nil {
		view.Fragment.Content = rewrite(view.Fragment.Content)
	}
	if view.Wiki != nil {
		view.Wiki.Content = rewrite(view.Wiki.Content)
		view.Wiki.Summary = rewrite(view.Wiki.Summary)
	}
}

func isReferenceNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
