package wikicontract

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func mutationDB(t *testing.T) (*gorm.DB, *types.WikiPage) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:wiki-mutation-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.AutoMigrate(&types.KnowledgeBase{}, &types.WikiPage{}, &types.WikiPageIssue{}); err != nil {
		t.Fatal(err)
	}
	for _, kb := range []string{"a", "b"} {
		if err = db.Create(&types.KnowledgeBase{ID: kb, TenantID: 1, Name: kb}).Error; err != nil {
			t.Fatal(err)
		}
		if err = db.Create(&types.WikiPage{ID: "page-" + kb, KnowledgeBaseID: kb, TenantID: 1, Slug: "same", Version: 3, Title: "Original", Content: "original", SourceRefs: types.StringArray{"doc-" + kb}}).Error; err != nil {
			t.Fatal(err)
		}
	}
	page := new(types.WikiPage)
	if err = db.First(page, "id = ?", "page-b").Error; err != nil {
		t.Fatal(err)
	}
	return db, page
}

func TestRepairWikiTargetRejectsAmbiguityAndStaleRead(t *testing.T) {
	if _, err := (Target{}).KnowledgeBase([]string{"a", "b"}); err == nil {
		t.Fatal("ambiguous KB accepted")
	}
	if _, err := (Target{KnowledgeBaseID: "c"}).KnowledgeBase([]string{"a", "b"}); err == nil {
		t.Fatal("outside KB accepted")
	}
	v := 2
	target := Target{PageID: "p", ExpectedVersion: &v}
	if err := target.ValidatePage(&types.WikiPage{ID: "p", Version: 3}, true); err == nil {
		t.Fatal("stale read accepted")
	}
	v = 3
	if err := target.ValidatePage(&types.WikiPage{ID: "other", Version: 3}, true); err == nil {
		t.Fatal("recreated slug accepted")
	}
}

func TestRepairWikiRenameKeepsIdentityAndUpdatesLinksAtomically(t *testing.T) {
	db, page := mutationDB(t)
	if err := db.Create(&types.WikiPage{ID: "link", TenantID: 1, KnowledgeBaseID: "b", Slug: "link", Version: 1, Content: "[[same|Label]]", OutLinks: types.StringArray{"same"}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&types.WikiPageIssue{ID: "issue", TenantID: 1, KnowledgeBaseID: "b", Slug: "same"}).Error; err != nil {
		t.Fatal(err)
	}
	next := "renamed"
	if err := MutateIdentity(context.Background(), db, page, &next); err != nil {
		t.Fatal(err)
	}
	var got types.WikiPage
	db.First(&got, "id = ?", "page-b")
	if got.Slug != "renamed" || got.Version != 4 || len(got.SourceRefs) != 1 || got.SourceRefs[0] != "doc-b" {
		t.Fatalf("lost identity/provenance: %+v", got)
	}
	got = types.WikiPage{}
	db.First(&got, "id = ?", "page-a")
	if got.Slug != "same" || got.Version != 3 {
		t.Fatal("other KB mutated")
	}
	got = types.WikiPage{}
	db.First(&got, "id = ?", "link")
	if got.Content != "[[renamed|Label]]" || got.OutLinks[0] != "renamed" {
		t.Fatal("links not committed")
	}
	var issue types.WikiPageIssue
	db.First(&issue, "id = ?", "issue")
	if issue.Slug != "renamed" {
		t.Fatal("issue identity stale")
	}
	if err := MutateIdentity(context.Background(), db, page, nil); err == nil {
		t.Fatal("stale delete accepted")
	}
}

func TestRepairWikiConcurrentIdentityWritesHaveOneWinner(t *testing.T) {
	db, page := mutationDB(t)
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for _, name := range []string{"next-a", "next-b"} {
		wg.Add(1)
		go func(name string) { defer wg.Done(); out <- MutateIdentity(context.Background(), db, page, &name) }(name)
	}
	wg.Wait()
	close(out)
	success := 0
	for err := range out {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful writers=%d", success)
	}
}
