package repository

import (
	"context"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type fanoutCompletionRepository interface {
	RecordKnowledgeFanoutCompletion(context.Context, uint64, string, string, string, string) (bool, error)
	ListKnowledgeFanoutCompletions(context.Context, uint64, string, string, string) ([]string, error)
	CountKnowledgeFanoutCompletions(context.Context, uint64, string, string, string) (int64, error)
	KnowledgeFanoutCompletionExists(context.Context, uint64, string, string, string, string) (bool, error)
	CleanupKnowledgeFanoutCompletions(context.Context, uint64, string, string, string) error
	FinalizeSubtaskGenerationItem(context.Context, uint64, string, string, string, string) (int, bool, error)
}

func newFanoutCompletionRepository(t *testing.T) (*gorm.DB, fanoutCompletionRepository) {
	t.Helper()
	dsn := fmt.Sprintf("file:fanout-completion-%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&types.Knowledge{}, &types.KnowledgeFanoutCompletion{}); err != nil {
		t.Fatal(err)
	}
	repo, ok := NewKnowledgeRepository(db).(fanoutCompletionRepository)
	if !ok {
		t.Fatal("knowledge repository does not expose durable fanout completion methods")
	}
	return db, repo
}

func insertFanoutKnowledge(t *testing.T, db *gorm.DB, generation, status string, pending int) {
	t.Helper()
	knowledge := &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          status,
		ProcessingGeneration: generation,
		PendingSubtasksCount: pending,
	}
	if err := db.Create(knowledge).Error; err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeFanoutCompletionIsGenerationScopedAndIdempotent(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusProcessing, 0)
	ctx := context.Background()
	inserted, err := repo.RecordKnowledgeFanoutCompletion(ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0")
	if err != nil || !inserted {
		t.Fatalf("first insert = inserted:%v err:%v", inserted, err)
	}
	inserted, err = repo.RecordKnowledgeFanoutCompletion(ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0")
	if err != nil || inserted {
		t.Fatalf("duplicate insert = inserted:%v err:%v, want false/nil", inserted, err)
	}
	if err := db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Update("processing_generation", "generation-2").Error; err != nil {
		t.Fatal(err)
	}
	if inserted, err := repo.RecordKnowledgeFanoutCompletion(ctx, 42, "knowledge-1", "kb-1", "generation-2", "image:1"); err != nil || !inserted {
		t.Fatal(err)
	}
	items, err := repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0] != "image:0" {
		t.Fatalf("generation-1 items = %v, want [image:0]", items)
	}
	count, err := repo.CountKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-2")
	if err != nil || count != 1 {
		t.Fatalf("generation-2 count = %d err=%v, want 1/nil", count, err)
	}
	exists, err := repo.KnowledgeFanoutCompletionExists(ctx, 42, "knowledge-1", "kb-1", "generation-2", "image:1")
	if err != nil || !exists {
		t.Fatalf("generation-2 image exists = %v err=%v, want true/nil", exists, err)
	}
}

func TestKnowledgeFanoutCompletionRetentionAndStaleWorkerFence(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusProcessing, 0)
	ctx := context.Background()
	if inserted, err := repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0",
	); err != nil || !inserted {
		t.Fatalf("record old generation = %v/%v", inserted, err)
	}
	if err := db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Update("processing_generation", "generation-2").Error; err != nil {
		t.Fatal(err)
	}
	if inserted, err := repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-2", "image:1",
	); err != nil || !inserted {
		t.Fatalf("record current generation = %v/%v", inserted, err)
	}
	if err := repo.CleanupKnowledgeFanoutCompletions(
		ctx, 42, "knowledge-1", "kb-1", "generation-2",
	); err != nil {
		t.Fatal(err)
	}
	old, err := repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-1")
	if err != nil || len(old) != 0 {
		t.Fatalf("old generation after retention cleanup = %v err=%v", old, err)
	}
	current, err := repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-2")
	if err != nil || len(current) != 1 || current[0] != "image:1" {
		t.Fatalf("current generation after retention cleanup = %v err=%v", current, err)
	}

	// A delayed task from the deleted generation must not recreate the row.
	inserted, err := repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0",
	)
	if err != nil || inserted {
		t.Fatalf("stale delayed completion = inserted:%v err:%v, want false/nil", inserted, err)
	}
	old, err = repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-1")
	if err != nil || len(old) != 0 {
		t.Fatalf("stale generation was recreated = %v err=%v", old, err)
	}

	if err := repo.CleanupKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", ""); err != nil {
		t.Fatal(err)
	}
	current, err = repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-2")
	if err != nil || len(current) != 0 {
		t.Fatalf("all-generation cleanup = %v err=%v", current, err)
	}
}

func TestFinalizeSubtaskGenerationItemDrainsExactlyOnce(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusFinalizing, 2)
	ctx := context.Background()

	count, promoted, err := repo.FinalizeSubtaskGenerationItem(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "summary",
	)
	if err != nil || promoted || count != 1 {
		t.Fatalf("first summary drain = count:%d promoted:%v err:%v", count, promoted, err)
	}
	count, promoted, err = repo.FinalizeSubtaskGenerationItem(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "summary",
	)
	if err != nil || promoted || count != 1 {
		t.Fatalf("duplicate summary drain = count:%d promoted:%v err:%v", count, promoted, err)
	}
	count, promoted, err = repo.FinalizeSubtaskGenerationItem(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "question_batch[0]",
	)
	if err != nil || !promoted || count != 0 {
		t.Fatalf("final distinct drain = count:%d promoted:%v err:%v", count, promoted, err)
	}
	var knowledge types.Knowledge
	if err := db.Where("id = ?", "knowledge-1").Take(&knowledge).Error; err != nil {
		t.Fatal(err)
	}
	if knowledge.ParseStatus != types.ParseStatusCompleted || knowledge.PendingSubtasksCount != 0 {
		t.Fatalf("knowledge lifecycle = status:%s pending:%d", knowledge.ParseStatus, knowledge.PendingSubtasksCount)
	}
}

func TestDeleteKnowledgeSoftDeleteExplicitlyClearsFanoutLedger(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusProcessing, 0)
	ctx := context.Background()
	if inserted, err := repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0",
	); err != nil || !inserted {
		t.Fatalf("record completion = %v/%v", inserted, err)
	}
	baseRepo := NewKnowledgeRepository(db)
	if err := baseRepo.DeleteKnowledge(ctx, 42, "knowledge-1"); err != nil {
		t.Fatal(err)
	}
	items, err := repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-1")
	if err != nil || len(items) != 0 {
		t.Fatalf("ledger after soft delete = %v err=%v", items, err)
	}
	var active int64
	if err := db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").Count(&active).Error; err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active knowledge rows = %d, want soft-deleted", active)
	}
}
