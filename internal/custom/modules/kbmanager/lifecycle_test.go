package kbmanager

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func replacementInput() ReplaceDocumentRequest {
	return ReplaceDocumentRequest{KnowledgeID: "old-doc", ExpectedOldFileHash: "old-hash", Source: FileSource{SourceType: "artifact", SourceID: "artifact-1", FileName: "replacement.md"}}
}

func TestRepairAcceptedInputSurvivesRunAndProcessLoss(t *testing.T) {
	s, k, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusCompleted)
	s.Stop()
	actor, err := actorFromContext(ctx, scope)
	require.NoError(t, err)
	kb, _, err := s.authorizeMutation(ctx, "kb-a")
	require.NoError(t, err)
	data := []byte("replacement content")
	op := s.newOperation(actor, kb, OperationTypeReplace, replacementInput().Source, "replacement.md", fmt.Sprintf("%x", sha256.Sum256(data)), "")
	op.OldKnowledgeID = "old-doc"
	op.OldFileHash = "old-hash"
	_, fresh, err := s.reserve(ctx, op, data)
	require.NoError(t, err)
	require.True(t, fresh)
	// Simulate the process disappearing immediately after accepting the input,
	// before calling native ingestion. The run-local resolver no longer exists.
	s.fileResolver = nil
	require.NoError(t, s.db.Model(&Operation{}).Where("id = ?", op.ID).Updates(map[string]any{"lease_until": time.Now().Add(-time.Second)}).Error)
	s.resumePending(ctx)
	require.True(t, k.exists("old-doc"), "preparation checkpoint must retain the published document")
	s.resumePending(ctx)
	var saved Operation
	require.NoError(t, s.db.First(&saved, "id = ?", op.ID).Error)
	require.Equal(t, OperationStateCompleted, saved.State)
	require.True(t, k.exists("new-doc"))
	require.False(t, k.exists("old-doc"))
	var remaining int64
	require.NoError(t, s.db.Model(&OperationInput{}).Where("operation_id = ?", op.ID).Count(&remaining).Error)
	require.Zero(t, remaining)
}

func TestRepairInputAndAcceptanceCommitTogether(t *testing.T) {
	s, _, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusProcessing)
	s.Stop()
	actor, err := actorFromContext(ctx, scope)
	require.NoError(t, err)
	kb, _, err := s.authorizeMutation(ctx, "kb-a")
	require.NoError(t, err)
	op := s.newOperation(actor, kb, OperationTypeAdd, replacementInput().Source, "replacement.md", "sha", "")
	require.NoError(t, s.db.Migrator().DropTable(&OperationInput{}))
	_, _, err = s.reserve(ctx, op, []byte("data"))
	require.Error(t, err)
	var count int64
	require.NoError(t, s.db.Model(&Operation{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestRepairReplacementFailurePreservesOld(t *testing.T) {
	for _, state := range []string{types.ParseStatusFailed, types.ParseStatusCancelled} {
		t.Run(state, func(t *testing.T) {
			s, k, scope, ctx := newKBManagerWorkflowService(t, state)
			op, err := s.ReplaceDocument(ctx, scope, replacementInput())
			if err != nil {
				t.Fatal(err)
			}
			got := waitForKBManagerOperation(t, s, ctx, scope, op.ID)
			if got.State != OperationStateFailed || !k.exists("old-doc") || k.wasDeleted("old-doc") {
				t.Fatalf("failure lost old document: %+v", got)
			}
		})
	}
}

func TestRepairReplacementCoreReadinessDoesNotWaitForOptionalEnrichment(t *testing.T) {
	s, k, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusProcessing)
	op, err := s.ReplaceDocument(ctx, scope, replacementInput())
	if err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	k.documents["new-doc"].CoreStatus = types.CoreStatusReady
	k.documents["new-doc"].EnrichmentStatus = types.EnrichmentStatusPending
	k.mu.Unlock()
	s.kick(op.ID)
	got := waitForKBManagerOperation(t, s, ctx, scope, op.ID)
	if !got.Searchable || got.EnrichmentStatus != types.EnrichmentStatusPending || k.exists("old-doc") {
		t.Fatalf("lifecycle conflated readiness/enrichment: %+v", got)
	}
}

func TestRepairReplacementRetryUsesSameOperation(t *testing.T) {
	s, _, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusProcessing)
	a, err := s.ReplaceDocument(ctx, scope, replacementInput())
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ReplaceDocument(ctx, scope, replacementInput())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatal("retry created another operation")
	}
}

func TestRepairReplacementRechecksInspectedHash(t *testing.T) {
	s, k, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusProcessing)
	op, err := s.ReplaceDocument(ctx, scope, replacementInput())
	if err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	k.documents["old-doc"].FileHash = "concurrent-edit"
	k.documents["new-doc"].CoreStatus = types.CoreStatusReady
	k.mu.Unlock()
	s.kick(op.ID)
	got := waitForKBManagerOperation(t, s, ctx, scope, op.ID)
	if got.State != OperationStateFailed || !k.exists("old-doc") {
		t.Fatal("concurrent old edit was deleted")
	}
}

func TestRepairReplacementResumesAfterCoordinatorRestart(t *testing.T) {
	s, k, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusProcessing)
	op, err := s.ReplaceDocument(ctx, scope, replacementInput())
	if err != nil {
		t.Fatal(err)
	}
	s.Stop()
	k.setStatus("new-doc", types.ParseStatusCompleted)
	other := NewService(s.db, s.kbService, k, s.kbShareService, s.tenantService, s.fileResolver)
	t.Cleanup(other.Stop)
	other.Start()
	got := waitForKBManagerOperation(t, other, ctx, scope, op.ID)
	if got.State != OperationStateCompleted || k.exists("old-doc") {
		t.Fatal("restart did not finish durable operation")
	}
}

func TestRepairReplacementLeaseExcludesOtherWorker(t *testing.T) {
	s, k, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusProcessing)
	op, err := s.ReplaceDocument(ctx, scope, replacementInput())
	if err != nil {
		t.Fatal(err)
	}
	s.Stop()
	k.setStatus("new-doc", types.ParseStatusCompleted)
	future := time.Now().Add(time.Hour)
	s.db.Model(&Operation{}).Where("id = ?", op.ID).Updates(map[string]any{"lease_owner": "another-worker", "lease_until": future})
	s.resumePending(context.Background())
	if !k.exists("old-doc") {
		t.Fatal("live lease stolen")
	}
	past := time.Now().Add(-time.Second)
	s.db.Model(&Operation{}).Where("id = ?", op.ID).Update("lease_until", past)
	s.resumePending(context.Background())
	if k.exists("old-doc") {
		t.Fatal("expired lease not recovered")
	}
}
