package imoutput

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type referenceTestKB struct {
	ID        string `gorm:"primaryKey"`
	TenantID  uint64
	DeletedAt gorm.DeletedAt
}

func (referenceTestKB) TableName() string { return "knowledge_bases" }

type referenceTestKnowledge struct {
	ID              string `gorm:"primaryKey"`
	TenantID        uint64
	KnowledgeBaseID string
	Title           string
	FileName        string
	FileType        string
	FileSize        int64
	EnableStatus    string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       gorm.DeletedAt
}

func (referenceTestKnowledge) TableName() string { return "knowledges" }

type referenceTestChunk struct {
	ID              string `gorm:"primaryKey"`
	TenantID        uint64
	KnowledgeID     string
	KnowledgeBaseID string
	Content         string
	ChunkIndex      int
	StartAt         int
	EndAt           int
	ChunkType       string
	SourceLocator   types.JSON `gorm:"type:text"`
	IsEnabled       bool
	Status          int
	DeletedAt       gorm.DeletedAt
}

func (referenceTestChunk) TableName() string { return "chunks" }

type referenceTestWiki struct {
	ID              string `gorm:"primaryKey"`
	TenantID        uint64
	KnowledgeBaseID string
	Slug            string
	Title           string
	PageType        string
	Status          string
	Summary         string
	Content         string
	Version         int
	UpdatedAt       time.Time
	DeletedAt       gorm.DeletedAt
}

func (referenceTestWiki) TableName() string { return "wiki_pages" }

type referenceTestFiles struct {
	tenantID uint64
	id       string
}

func (f *referenceTestFiles) GetKnowledgeFile(ctx context.Context, id string) (io.ReadCloser, string, error) {
	f.tenantID, _ = ctx.Value(types.TenantIDContextKey).(uint64)
	f.id = id
	return io.NopCloser(strings.NewReader("original document")), "制度.pdf", nil
}

func TestReferenceServiceResolvesOnlyCapabilityBoundDocumentAndOriginal(t *testing.T) {
	service, db, files := newReferenceTestService(t)
	token := issueServiceToken(t, service.signer, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-1", ChunkID: "chunk-1",
	}, 7)
	view, err := service.Resolve(context.Background(), token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if view.Type != sourcerefs.SourceTypeKnowledge || view.Document == nil || view.Fragment == nil ||
		view.Fragment.Content != "精确分片正文" || view.Fragment.ChunkIndex != 3 ||
		view.Document.FileName != "制度.pdf" || string(view.Fragment.SourceLocator) != `{"page":9}` {
		t.Fatalf("unexpected view: %#v", view)
	}

	file, err := service.OpenOriginal(context.Background(), token)
	if err != nil {
		t.Fatalf("OpenOriginal: %v", err)
	}
	body, _ := io.ReadAll(file.Reader)
	_ = file.Reader.Close()
	if string(body) != "original document" || files.tenantID != 7 || files.id != "doc-1" {
		t.Fatalf("original scope mismatch body=%q tenant=%d id=%q", body, files.tenantID, files.id)
	}

	if err := db.Model(&referenceTestChunk{}).Where("id = ?", "chunk-1").Update("is_enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(context.Background(), token); err == nil {
		t.Fatal("disabled source still resolved through an old capability")
	}
}

func TestReferenceServiceResolvesOnlyPublishedWikiInOwningTenantAndKB(t *testing.T) {
	service, db, _ := newReferenceTestService(t)
	token := issueServiceToken(t, service.signer, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeWiki, KnowledgeBaseID: "kb-1", Slug: "concept/rag",
	}, 7)
	view, err := service.Resolve(context.Background(), token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if view.Type != sourcerefs.SourceTypeWiki || view.Wiki == nil || view.Wiki.Title != "RAG" || view.Wiki.Content != "Wiki 正文" {
		t.Fatalf("unexpected wiki view: %#v", view)
	}
	if _, err := service.OpenOriginal(context.Background(), token); err == nil {
		t.Fatal("Wiki capability unexpectedly granted document access")
	}

	if err := db.Model(&referenceTestWiki{}).Where("slug = ?", "concept/rag").Update("status", types.WikiPageStatusArchived).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(context.Background(), token); err == nil {
		t.Fatal("archived Wiki still resolved through an old capability")
	}
}

func TestReferenceServiceRejectsCrossTenantAndCrossResourceCapabilities(t *testing.T) {
	service, _, _ := newReferenceTestService(t)
	for _, sourceAndTenant := range []struct {
		source *sourcerefs.CitationSource
		tenant uint64
	}{
		{&sourcerefs.CitationSource{Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-1", ChunkID: "chunk-1"}, 8},
		{&sourcerefs.CitationSource{Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-2", KnowledgeID: "doc-1", ChunkID: "chunk-1"}, 7},
		{&sourcerefs.CitationSource{Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-2", ChunkID: "chunk-1"}, 7},
	} {
		token := issueServiceToken(t, service.signer, sourceAndTenant.source, sourceAndTenant.tenant)
		if _, err := service.Resolve(context.Background(), token); err == nil {
			t.Fatalf("cross-scope token resolved: tenant=%d source=%#v", sourceAndTenant.tenant, sourceAndTenant.source)
		}
	}
}

func TestReferenceServiceRefreshesOnlyAssetsEmbeddedInAuthorizedContent(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", string(referenceTestKey))
	service, db, _ := newReferenceTestService(t)
	service.publicBaseURL = "https://knora.example.com"
	if err := db.Model(&referenceTestChunk{}).Where("id = ?", "chunk-1").
		Update("content", "证据图 ![流程](local://7/chunks/flow.png)").Error; err != nil {
		t.Fatal(err)
	}
	token := issueServiceToken(t, service.signer, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-1", ChunkID: "chunk-1",
	}, 7)
	view, err := service.Resolve(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	content := view.Fragment.Content
	start := strings.Index(content, "https://")
	end := strings.LastIndex(content, ")")
	if start < 0 || end <= start || strings.Contains(content, "local://") {
		t.Fatalf("provider asset was not converted on read: %s", content)
	}
	assetURL, err := url.Parse(content[start:end])
	if err != nil {
		t.Fatal(err)
	}
	query := assetURL.Query()
	if assetURL.Path != "/api/v1/files/presigned" || query.Get("tenant_id") != "7" ||
		query.Get("file_path") != "local://7/chunks/flow.png" ||
		!utils.VerifyFileURLSig(query.Get("file_path"), 7, query.Get("expires"), query.Get("sig")) {
		t.Fatalf("invalid scoped asset URL: %s", assetURL)
	}
}

func newReferenceTestService(t *testing.T) (*ReferenceService, *gorm.DB, *referenceTestFiles) {
	t.Helper()
	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&referenceTestKB{}, &referenceTestKnowledge{}, &referenceTestChunk{}, &referenceTestWiki{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	fixtures := []any{
		&referenceTestKB{ID: "kb-1", TenantID: 7},
		&referenceTestKnowledge{ID: "doc-1", TenantID: 7, KnowledgeBaseID: "kb-1", Title: "采购制度", FileName: "制度.pdf", FileType: "pdf", FileSize: 128, EnableStatus: "enabled", CreatedAt: now, UpdatedAt: now},
		&referenceTestChunk{ID: "chunk-1", TenantID: 7, KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", Content: "精确分片正文", ChunkIndex: 3, StartAt: 20, EndAt: 40, ChunkType: "text", SourceLocator: types.JSON(`{"page":9}`), IsEnabled: true, Status: int(types.ChunkStatusIndexed)},
		&referenceTestWiki{ID: "wiki-1", TenantID: 7, KnowledgeBaseID: "kb-1", Slug: "concept/rag", Title: "RAG", PageType: "concept", Status: types.WikiPageStatusPublished, Summary: "摘要", Content: "Wiki 正文", Version: 2, UpdatedAt: now},
	}
	for _, fixture := range fixtures {
		if err := db.Create(fixture).Error; err != nil {
			t.Fatal(err)
		}
	}
	files := &referenceTestFiles{}
	service := NewReferenceService(db, files, NewReferenceSigner(referenceTestKey))
	return service, db, files
}

func issueServiceToken(t *testing.T, signer *ReferenceSigner, source *sourcerefs.CitationSource, tenantID uint64) string {
	t.Helper()
	token, err := signer.Issue(source, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
