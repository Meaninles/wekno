package kbmanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func publicationPostgres(t *testing.T) (*Service, []*Operation) {
	db := testsupport.Postgres(t, &types.KnowledgeBase{}, &types.Knowledge{}, &types.WikiPage{}, &types.TaskPendingOp{}, &Operation{}, &OperationInput{})
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb", TenantID: 1, Name: "isolated"}).Error)
	require.NoError(t, db.Create(&types.Knowledge{ID: "old", TenantID: 1, KnowledgeBaseID: "kb", FileHash: "old-hash", PublicationState: "published", ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "old-generation", CoreStatus: types.CoreStatusReady}).Error)
	require.NoError(t, db.Create(&types.WikiPage{ID: "old-page", TenantID: 1, KnowledgeBaseID: "kb", Slug: "source-page", PageType: types.WikiPageTypeConcept, Status: types.WikiPageStatusPublished, Version: 4, SourceRefs: types.StringArray{"old|old-generation"}, Content: "prior materialized claims"}).Error)
	ops := []*Operation{}
	for _, id := range []string{"one", "two"} {
		require.NoError(t, db.Create(&types.Knowledge{ID: id, TenantID: 1, KnowledgeBaseID: "kb", PublicationState: "staged", CoreStatus: types.CoreStatusReady, ProcessingGeneration: id}).Error)
		op := &Operation{ID: id, Type: OperationTypeReplace, State: OperationStateParsing, SourceTenantID: 1, KnowledgeBaseID: "kb", OldKnowledgeID: "old", OldFileHash: "old-hash", NewKnowledgeID: id, NewGeneration: id, LeaseOwner: "worker-" + id}
		require.NoError(t, db.Create(op).Error)
		ops = append(ops, op)
	}
	return &Service{db: db}, ops
}

func publishedIDs(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var ids []string
	require.NoError(t, db.Model(&types.Knowledge{}).Where("publication_state = ?", "published").Order("id").Pluck("id", &ids).Error)
	return ids
}

func TestRepairPostgresOnlyOneConcurrentReplacementPublishes(t *testing.T) {
	s, ops := publicationPostgres(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, op := range ops {
		workers.Add(1)
		go func(op *Operation) {
			defer workers.Done()
			<-start
			results <- s.publishReplacement(context.Background(), op)
		}(op)
	}
	close(start)
	workers.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrPublicationConflict) {
			conflicted++
		} else {
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, conflicted)
	ids := publishedIDs(t, s.db)
	require.Len(t, ids, 1)
	require.NotEqual(t, "old", ids[0])
	var page types.WikiPage
	require.NoError(t, s.db.First(&page, "id = ?", "old-page").Error)
	require.Equal(t, types.WikiPageStatusArchived, page.Status)
	require.Equal(t, 5, page.Version)
	require.Equal(t, types.StringArray{"old|old-generation"}, page.SourceRefs)
}

func TestRepairPostgresPublicationRollsBackEveryBoundary(t *testing.T) {
	for _, scenario := range []string{"lease_lost", "write_failure", "wiki_write_failure", "tenant_changed", "kb_deleted", "generation_changed"} {
		t.Run(scenario, func(t *testing.T) {
			s, ops := publicationPostgres(t)
			op := ops[0]
			switch scenario {
			case "wiki_write_failure":
				require.NoError(t, s.db.Exec(`CREATE FUNCTION reject_quarantine() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected quarantine failure'; END $$`).Error)
				require.NoError(t, s.db.Exec(`CREATE TRIGGER fail_quarantine BEFORE UPDATE ON wiki_pages FOR EACH ROW EXECUTE FUNCTION reject_quarantine()`).Error)
			case "lease_lost":
				require.NoError(t, s.db.Model(&Operation{}).Where("id = ?", op.ID).Update("lease_owner", "successor").Error)
			case "write_failure":
				require.NoError(t, s.db.Exec(`CREATE FUNCTION reject_publish() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.publication_state = 'published' THEN RAISE EXCEPTION 'injected publication failure'; END IF; RETURN NEW; END $$`).Error)
				require.NoError(t, s.db.Exec(`CREATE TRIGGER fail_publish BEFORE UPDATE ON knowledges FOR EACH ROW EXECUTE FUNCTION reject_publish()`).Error)
			case "tenant_changed":
				op.SourceTenantID = 2
			case "kb_deleted":
				require.NoError(t, s.db.Where("id = ?", "kb").Delete(&types.KnowledgeBase{}).Error)
			case "generation_changed":
				op.NewGeneration = "other"
			}
			require.Error(t, s.publishReplacement(context.Background(), op))
			require.Equal(t, []string{"old"}, publishedIDs(t, s.db))
			var page types.WikiPage
			require.NoError(t, s.db.First(&page, "id = ?", "old-page").Error)
			require.Equal(t, types.WikiPageStatusPublished, page.Status)
			require.Equal(t, 4, page.Version)
			var stored Operation
			require.NoError(t, s.db.First(&stored, "id = ?", op.ID).Error)
			require.Equal(t, OperationStateParsing, stored.State)
		})
	}
}

func TestRepairPostgresCancelledPublicationLeavesOldAvailable(t *testing.T) {
	s, ops := publicationPostgres(t)
	tx := s.db.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()
	require.NoError(t, tx.Exec("SELECT id FROM knowledge_bases WHERE id = ? FOR UPDATE", "kb").Error)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.Error(t, s.publishReplacement(ctx, ops[0]))
	require.NoError(t, tx.Rollback().Error)
	require.Equal(t, []string{"old"}, publishedIDs(t, s.db))
	// A different worker can resume after the lost request, using the same
	// persisted operation and source identities.
	require.NoError(t, s.publishReplacement(context.Background(), ops[0]))
	require.Equal(t, []string{"one"}, publishedIDs(t, s.db))
}
