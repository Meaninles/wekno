package processownership

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestBatchReparseIdentityIsStableAndBatchScoped(t *testing.T) {
	generation, owner := BatchReparseIdentity(7, "batch-a", "knowledge-1")
	againGeneration, againOwner := BatchReparseIdentity(7, "batch-a", "knowledge-1")
	if generation == "" || owner == "" || generation != againGeneration || owner != againOwner {
		t.Fatalf("identity is not stable: (%q,%q) then (%q,%q)", generation, owner, againGeneration, againOwner)
	}
	newGeneration, newOwner := BatchReparseIdentity(7, "batch-b", "knowledge-1")
	if newGeneration == generation || newOwner == owner {
		t.Fatalf("new batch reused old identity: (%q,%q)", newGeneration, newOwner)
	}
	if want := DocumentOwner("knowledge-1", generation); owner != want {
		t.Fatalf("owner = %q, want %q", owner, want)
	}
}

func TestBatchReparseIdentityFailsClosedOnIncompleteInput(t *testing.T) {
	for _, tc := range []struct {
		tenant uint64
		batch  string
		id     string
	}{
		{batch: "batch", id: "knowledge"},
		{tenant: 1, id: "knowledge"},
		{tenant: 1, batch: "batch"},
	} {
		if generation, owner := BatchReparseIdentity(tc.tenant, tc.batch, tc.id); generation != "" || owner != "" {
			t.Fatalf("incomplete identity produced generation=%q owner=%q", generation, owner)
		}
	}
}

func TestBatchReparseSnapshotSurvivesJSONAndFencesUpdatedAtABA(t *testing.T) {
	updatedAt := time.Date(2026, 7, 22, 1, 2, 3, 456789000, time.FixedZone("CST", 8*60*60))
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "generation-old",
		ProcessingOwner: "", UpdatedAt: updatedAt,
	}
	snapshot, err := CaptureBatchReparseSnapshot(knowledge)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded types.KnowledgeReparseExpectedSnapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !BatchReparseSnapshotMatches(knowledge, decoded) {
		t.Fatalf("JSON round-trip changed snapshot: %s", raw)
	}
	knowledge.UpdatedAt = knowledge.UpdatedAt.Add(time.Microsecond)
	if BatchReparseSnapshotMatches(knowledge, decoded) {
		t.Fatal("same status/generation/owner with a newer UpdatedAt passed the ABA fence")
	}
}
