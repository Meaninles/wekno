package grepsearch

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func fixture(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv("GREPSEARCH_TEST_POSTGRES") != "1" {
		t.Skip("set GREPSEARCH_TEST_POSTGRES=1 in the local runtime container")
	}
	dsn, err := database.PostgresGormDSNFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	root, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("grep_test_%d", time.Now().UnixNano())
	must(t, root, "CREATE SCHEMA "+name)
	db, err := gorm.Open(postgres.Open(dsn+" search_path="+name), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !strings.HasPrefix(name, "grep_test_") {
			panic("unsafe test schema")
		}
		root.Exec("DROP SCHEMA " + name + " CASCADE")
		if c, e := db.DB(); e == nil {
			c.Close()
		}
		if c, e := root.DB(); e == nil {
			c.Close()
		}
	})
	must(t, db, `CREATE TABLE knowledges(id varchar(36) PRIMARY KEY,title text,publication_state text,deleted_at timestamptz);
        CREATE TABLE chunks(id varchar(36) PRIMARY KEY,knowledge_id varchar(36),knowledge_base_id varchar(36),tenant_id bigint,
        created_at timestamptz,content text,is_enabled boolean,chunk_type text,deleted_at timestamptz,
        chunk_index bigint,metadata jsonb,source_locator jsonb);
        CREATE TABLE knowledge_tag_relations(knowledge_id text,tag_id text);
        INSERT INTO knowledges VALUES('doc','标题独占探针','published',NULL),('other','Other','published',NULL);
        INSERT INTO chunks SELECT md5(g::text),'doc','kb',7,'2026-01-01','合同金额 '||g,true,'text',NULL,g,
            '{"kind":"fixture"}', '{"page":1}' FROM generate_series(1,600) g;`)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func must(t *testing.T, db *gorm.DB, q string, args ...interface{}) {
	t.Helper()
	if err := db.Exec(q, args...).Error; err != nil {
		t.Fatal(err)
	}
}

func assertProjection(t *testing.T, db *gorm.DB) {
	t.Helper()
	var mismatch int64
	err := db.Raw(`WITH expected AS (
        SELECT c.id,c.knowledge_id,c.knowledge_base_id,c.tenant_id,c.created_at,c.content,k.title AS knowledge_title
        FROM chunks c JOIN knowledges k ON k.id=c.knowledge_id
        WHERE c.is_enabled AND c.deleted_at IS NULL AND c.chunk_type<>'summary' AND k.deleted_at IS NULL AND k.publication_state='published')
        SELECT count(*) FROM ((SELECT * FROM expected EXCEPT SELECT * FROM custom_grepsearch_chunks)
        UNION ALL (SELECT * FROM custom_grepsearch_chunks EXCEPT SELECT * FROM expected)) d`).Scan(&mismatch).Error
	if err != nil || mismatch != 0 {
		t.Fatalf("projection mismatch=%d err=%v", mismatch, err)
	}
}

func TestPostgresExactSearchAndMigration(t *testing.T) {
	db := fixture(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertProjection(t, db)
	for _, pattern := range []string{"标题独占探针", "合同金额", "标题独占探针|合同金额", "合同.{0,6}金额", ".*", "(合同)?", "不存在", "' OR true --"} {
		rows, err := Search(ctx, db, "chunks.knowledge_base_id = ? AND chunks.tenant_id = ?", []interface{}{"kb", 7}, []string{pattern})
		if err != nil {
			t.Fatal(err)
		}
		var want []Row
		err = db.Raw(`SELECT c.id,c.content,c.chunk_index,c.knowledge_id,c.knowledge_base_id,c.chunk_type,c.metadata,c.source_locator,c.created_at,k.title AS knowledge_title
            FROM chunks c JOIN knowledges k ON c.knowledge_id=k.id WHERE c.knowledge_base_id='kb' AND c.tenant_id=7
            AND c.is_enabled AND c.deleted_at IS NULL AND c.chunk_type<>'summary' AND k.deleted_at IS NULL AND k.publication_state='published'
            AND (c.content ~* ? OR k.title ~* ?) ORDER BY c.created_at DESC,c.id DESC LIMIT 500`, pattern, pattern).Scan(&want).Error
		if err != nil {
			t.Fatal(err)
		}
		for i := range want {
			want[i].TotalChunkCount = 600
		}
		if len(rows) != len(want) || (len(rows) > 0 && !reflect.DeepEqual(rows, want)) {
			t.Fatalf("different payload/count/order for %q: got=%d want=%d", pattern, len(rows), len(want))
		}
	}
	rows, err := Search(ctx, db, "chunks.knowledge_base_id=? AND chunks.tenant_id=?", []interface{}{"kb", 999}, []string{".*"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("tenant leak: %d %v", len(rows), err)
	}
	must(t, db, "INSERT INTO knowledge_tag_relations VALUES('doc','tag')")
	for _, scope := range []string{
		"chunks.knowledge_id='doc'",
		"chunks.knowledge_base_id='kb' AND chunks.tenant_id=7 AND EXISTS(SELECT 1 FROM knowledge_tag_relations t WHERE t.knowledge_id=chunks.knowledge_id AND t.tag_id='tag')",
		"(chunks.knowledge_base_id='none' AND chunks.tenant_id=7) OR chunks.knowledge_id='doc'",
	} {
		rows, err := Search(ctx, db, scope, nil, []string{".*"})
		if err != nil || len(rows) != 500 {
			t.Fatalf("scope rows=%d err=%v", len(rows), err)
		}
	}
	if _, err := Search(ctx, db, "true", nil, []string{"["}); err == nil {
		t.Fatal("invalid regex must fail, not look like zero hits")
	}
	must(t, db, "ALTER TABLE chunks DISABLE TRIGGER custom_grepsearch_chunks_update")
	if Validate(ctx, db) == nil {
		t.Fatal("disabled trigger must fail readiness")
	}
}

func TestPostgresBatchLifecycle(t *testing.T) {
	db := fixture(t)
	for _, q := range []string{
		"UPDATE knowledges SET title='新标题' WHERE id='doc'",
		"UPDATE knowledges SET publication_state='draft' WHERE id='doc'",
		"UPDATE knowledges SET publication_state='published' WHERE id='doc'",
		"UPDATE chunks SET is_enabled=false",
		"UPDATE chunks SET is_enabled=true",
		"UPDATE chunks SET deleted_at=now()",
		"UPDATE chunks SET deleted_at=NULL",
		"UPDATE chunks SET chunk_type='summary'",
		"UPDATE chunks SET chunk_type='text',content='新正文'",
		"UPDATE chunks SET knowledge_id='other',knowledge_base_id='moved',tenant_id=9 WHERE chunk_index<=300",
		"UPDATE chunks SET metadata='{}'",
		"DELETE FROM chunks WHERE chunk_index<100",
		"DELETE FROM knowledges WHERE id='other'",
	} {
		must(t, db, q)
		assertProjection(t, db)
	}
	tx := db.Begin()
	must(t, tx, "UPDATE chunks SET content='rolled back'")
	assertProjection(t, tx)
	tx.Rollback()
	assertProjection(t, db)
	must(t, db, "TRUNCATE chunks")
	assertProjection(t, db)
}

func TestPostgresConcurrentPublication(t *testing.T) {
	db := fixture(t)
	for _, firstDocument := range []bool{false, true} {
		must(t, db, "UPDATE knowledges SET publication_state='published' WHERE id='doc'")
		tx := db.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		first, second := "UPDATE chunks SET content=content||'x' WHERE chunk_index=1", "UPDATE knowledges SET publication_state='draft' WHERE id='doc'"
		if firstDocument {
			first, second = second, first
		}
		must(t, tx, first)
		done := make(chan error, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		go func() { done <- db.WithContext(ctx).Exec(second).Error }()
		// The second writer must wait for the shared document lock.
		select {
		case err := <-done:
			tx.Rollback()
			cancel()
			t.Fatalf("writer did not serialize: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if err := tx.Commit().Error; err != nil {
			cancel()
			t.Fatal(err)
		}
		err := <-done
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		assertProjection(t, db)
	}
}

func TestPostgresRejectsStaleSnapshotWrites(t *testing.T) {
	db := fixture(t)
	tx := db.Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	err := tx.Exec("INSERT INTO chunks(id,knowledge_id,knowledge_base_id,tenant_id,is_enabled,chunk_type) VALUES('new','doc','kb',7,true,'text')").Error
	tx.Rollback()
	if err == nil || !strings.Contains(err.Error(), "READ COMMITTED") {
		t.Fatalf("unsafe writer isolation must fail explicitly: %v", err)
	}
	assertProjection(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Search(ctx, db, "true", nil, []string{".*"}); err == nil {
		t.Fatal("cancelled search must fail")
	}
	if _, err := Search(context.Background(), db, "true", nil, []string{".*"}); err != nil {
		t.Fatalf("connection after cancellation: %v", err)
	}
}
