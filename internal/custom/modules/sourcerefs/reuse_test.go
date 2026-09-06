package sourcerefs

import (
	"context"
	"fmt"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestRepairEvidenceReuseRejectsChangedGenerationScopeAndContent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := db.DB()
	connection.SetMaxOpenConns(1)
	defer connection.Close()
	for _, sql := range []string{
		`CREATE TABLE chunks(id TEXT,tenant_id INTEGER,knowledge_base_id TEXT,knowledge_id TEXT,is_enabled BOOLEAN,content TEXT,chunk_type TEXT,processing_generation TEXT,updated_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE knowledges(id TEXT,tenant_id INTEGER,knowledge_base_id TEXT,enable_status TEXT,core_status TEXT,published_generation TEXT,deleted_at DATETIME)`,
		`INSERT INTO chunks VALUES('c',7,'kb','d',true,'exact source','text','g1',NULL,NULL)`,
		`INSERT INTO knowledges VALUES('d',7,'kb','enabled','ready','g1',NULL)`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	ref := &types.SearchResult{ID: "c", KnowledgeID: "d", KnowledgeBaseID: "kb", ChunkType: "text", Content: "exact source", EvidenceContent: "exact source"}
	AssignCitationIDs([]*types.SearchResult{ref})
	targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 7}}
	check := func(scope types.SearchTargets, want int) {
		t.Helper()
		got, invalid, err := ReuseEvidence(context.Background(), db, scope, []*types.SearchResult{ref})
		if err != nil || len(got) != want || len(invalid) != 1-want {
			t.Fatal(got, invalid, err)
		}
	}
	check(targets, 1)
	check(nil, 0)
	check(types.SearchTargets{{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", TenantID: 7, KnowledgeIDs: []string{"other"}}}, 0)
	for _, statement := range []string{`UPDATE knowledges SET published_generation='g2'`, `UPDATE chunks SET content='changed'`, `UPDATE chunks SET is_enabled=false`, `UPDATE chunks SET deleted_at=CURRENT_TIMESTAMP`} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
		check(targets, 0)
		db.Exec(`UPDATE chunks SET content='exact source',is_enabled=true,deleted_at=NULL`)
		db.Exec(`UPDATE knowledges SET published_generation='g1'`)
	}
	db.Exec(`UPDATE chunks SET updated_at=?`, time.Now().Add(time.Minute))
	check(targets, 0)
	if CitationID(ref) != "S1" || ref.EvidenceContent != "exact source" {
		t.Fatal("persisted evidence mutated")
	}
}

func TestEvidenceReuseBatchesFragmentsAndKeepsTagScope(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := db.DB()
	conn.SetMaxOpenConns(1)
	defer conn.Close()
	for _, statement := range []string{
		`CREATE TABLE chunks(id TEXT,tenant_id INTEGER,knowledge_base_id TEXT,knowledge_id TEXT,is_enabled BOOLEAN,content TEXT,chunk_type TEXT,processing_generation TEXT,tag_id TEXT,updated_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE knowledges(id TEXT,tenant_id INTEGER,knowledge_base_id TEXT,enable_status TEXT,core_status TEXT,published_generation TEXT,deleted_at DATETIME)`,
		`CREATE TABLE knowledge_tag_relations(knowledge_id TEXT,tag_id TEXT)`,
		`INSERT INTO knowledges VALUES('d',7,'kb','enabled','ready','g1',NULL)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	var refs []*types.SearchResult
	for i := 0; i < 20; i++ {
		id, text, tag := fmt.Sprint(i), fmt.Sprintf("exact text %d", i), "other"
		if i%2 == 0 {
			tag = "allowed"
		}
		if err := db.Exec(`INSERT INTO chunks VALUES(?,7,'kb','d',true,?,'text','g1',?,NULL,NULL)`, id, text, tag).Error; err != nil {
			t.Fatal(err)
		}
		refs = append(refs, &types.SearchResult{ID: id, KnowledgeID: "d", KnowledgeBaseID: "kb", ChunkType: "text", Content: text, EvidenceContent: text})
	}
	AssignCitationIDs(refs)
	queries := 0
	if err := db.Callback().Query().After("gorm:query").Register("count_queries", func(*gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	scope := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 7, TagIDs: []string{"allowed"}}}
	got, invalid, err := ReuseEvidence(context.Background(), db, scope, refs)
	if err != nil || len(got) != 10 || len(invalid) != 10 || queries != 2 {
		t.Fatal(len(got), len(invalid), queries, err)
	}
	db.Exec(`INSERT INTO knowledge_tag_relations VALUES('d','allowed')`)
	queries = 0
	got, invalid, err = ReuseEvidence(context.Background(), db, scope, refs)
	if err != nil || len(got) != 20 || len(invalid) != 0 || queries != 2 {
		t.Fatal(len(got), len(invalid), queries, err)
	}
	db.Exec(`UPDATE knowledges SET enable_status='disabled'`)
	got, invalid, err = ReuseEvidence(context.Background(), db, scope, refs)
	if err != nil || len(got) != 0 || len(invalid) != 20 {
		t.Fatal(len(got), len(invalid), err)
	}
}

func TestRepairWikiEvidenceReuseRequiresPageVersion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := db.DB()
	connection.SetMaxOpenConns(1)
	defer connection.Close()
	db.Exec(`CREATE TABLE wiki_pages(id TEXT,tenant_id INTEGER,knowledge_base_id TEXT,slug TEXT,title TEXT,summary TEXT,content TEXT,version INTEGER,page_type TEXT,deleted_at DATETIME)`)
	db.Exec(`INSERT INTO wiki_pages VALUES('page',7,'kb','entry','title','summary','body',1,'concept',NULL)`)
	ref := &types.SearchResult{ID: "wiki:kb:entry", KnowledgeBaseID: "kb", ChunkType: "wiki_page", Content: "summary\n\nbody", Metadata: map[string]string{"source_type": "wiki", "slug": "entry", "page_id": "page", "page_version": "1"}}
	AssignCitationIDs([]*types.SearchResult{ref})
	scope := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 7}}
	got, _, err := ReuseEvidence(context.Background(), db, scope, []*types.SearchResult{ref})
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	db.Exec(`UPDATE wiki_pages SET version=2`)
	got, invalid, err := ReuseEvidence(context.Background(), db, scope, []*types.SearchResult{ref})
	if err != nil || len(got) != 0 || len(invalid) != 1 {
		t.Fatal(got, invalid, err)
	}
}
