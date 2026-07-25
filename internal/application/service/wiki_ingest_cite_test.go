package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestCitationBatchResultAcceptsScalarListFields(t *testing.T) {
	t.Parallel()

	raw := `{
		"citations":{"entity/version-management":"c000, c001"},
		"new_slugs":[{
			"type":"concept",
			"name":"版本管理",
			"slug":"concept/version-management",
			"aliases":"版本管理, 版本迭代",
			"description":"版本管理流程",
			"details":"版本控制要求",
			"source_chunks":"c000"
		}]
	}`
	var got citationBatchResult
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !equalStrings([]string(got.Citations["entity/version-management"]), []string{"c000", "c001"}) {
		t.Fatalf("citations = %v", got.Citations)
	}
	if len(got.NewSlugs) != 1 ||
		!equalStrings([]string(got.NewSlugs[0].Aliases), []string{"版本管理", "版本迭代"}) ||
		!equalStrings([]string(got.NewSlugs[0].SourceChunks), []string{"c000"}) {
		t.Fatalf("new_slugs = %+v", got.NewSlugs)
	}
}

type citationThresholdChatModel struct{}

func (citationThresholdChatModel) Chat(
	_ context.Context,
	messages []chat.Message,
	_ *chat.ChatOptions,
) (*types.ChatResponse, error) {
	if len(messages) > 0 && strings.Contains(messages[0].Content, "FAIL_CITATION_BATCH") {
		return nil, errors.New("invalid arguments injected for citation batch")
	}
	return &types.ChatResponse{
		Content: `{"citations":{"entity/acme":["c000"]},"new_slugs":[]}`,
	}, nil
}

func (citationThresholdChatModel) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, errors.New("not used")
}

func (citationThresholdChatModel) GetModelName() string { return "citation-threshold" }
func (citationThresholdChatModel) GetModelID() string   { return "citation-threshold" }

// TestMergeCitationsIntoItems_PopulatesSourceChunksOnCandidates verifies that
// citations returned by the chunk-classification pass are attached back onto
// the matching candidate items while non-cited candidates are left untouched.
func TestMergeCitationsIntoItems_PopulatesSourceChunksOnCandidates(t *testing.T) {
	entities := []extractedItem{
		{Name: "Acme", Slug: "entity/acme"},
		{Name: "Beta", Slug: "entity/beta"},
	}
	concepts := []extractedItem{
		{Name: "RAG", Slug: "concept/rag"},
	}
	citations := map[string][]string{
		"entity/acme": {"chunk-1", "chunk-3"},
		"concept/rag": {"chunk-2"},
	}

	gotE, gotC, uncited := mergeCitationsIntoItems(entities, concepts, citations, nil)

	if len(gotE) != 2 || len(gotC) != 1 {
		t.Fatalf("expected 2 entities + 1 concept, got %d + %d", len(gotE), len(gotC))
	}
	acme := findBySlug(gotE, "entity/acme")
	if acme == nil {
		t.Fatalf("entity/acme missing from result")
	}
	if !equalStrings(acme.SourceChunks, []string{"chunk-1", "chunk-3"}) {
		t.Errorf("entity/acme source_chunks = %v, want [chunk-1 chunk-3]", acme.SourceChunks)
	}
	beta := findBySlug(gotE, "entity/beta")
	if beta == nil {
		t.Fatalf("entity/beta missing from result")
	}
	if len(beta.SourceChunks) != 0 {
		t.Errorf("entity/beta should have no citations, got %v", beta.SourceChunks)
	}
	rag := findBySlug(gotC, "concept/rag")
	if rag == nil {
		t.Fatalf("concept/rag missing")
	}
	if !equalStrings(rag.SourceChunks, []string{"chunk-2"}) {
		t.Errorf("concept/rag source_chunks = %v, want [chunk-2]", rag.SourceChunks)
	}
	if uncited != 1 {
		t.Errorf("uncited = %d, want 1", uncited)
	}
}

func TestClassifyChunkCitationsEnforcesFailureThreshold(t *testing.T) {
	makeChunks := func(count int) []*types.Chunk {
		chunks := make([]*types.Chunk, 0, count)
		for index := 0; index < count; index++ {
			content := strings.Repeat("ordinary citation content ", 700)
			if index == 0 {
				content = "FAIL_CITATION_BATCH " + content
			}
			chunks = append(chunks, &types.Chunk{
				ID:         string(rune('a' + index)),
				ChunkIndex: index,
				ChunkType:  types.ChunkTypeText,
				Content:    content,
			})
		}
		return chunks
	}
	service := &wikiIngestService{}
	candidates := `- slug: entity/acme, type: entity, name: "Acme"`

	_, _, attempted, failed, err := service.classifyChunkCitations(
		context.Background(),
		citationThresholdChatModel{},
		candidates,
		makeChunks(5),
		"English",
		&WikiBatchContext{},
	)
	if err == nil || attempted != 5 || failed != 1 {
		t.Fatalf(
			"threshold result = attempted:%d failed:%d err:%v, want 5/1/error",
			attempted, failed, err,
		)
	}

	_, _, attempted, failed, err = service.classifyChunkCitations(
		context.Background(),
		citationThresholdChatModel{},
		candidates,
		makeChunks(6),
		"English",
		&WikiBatchContext{},
	)
	if err != nil || attempted != 6 || failed != 1 {
		t.Fatalf(
			"degraded result = attempted:%d failed:%d err:%v, want 6/1/nil",
			attempted, failed, err,
		)
	}
}

// TestMergeCitationsIntoItems_AddsNewSlugsAndUnionsChunksAcrossBatches checks
// that genuinely new slugs (ones Pass 0 missed) are appended to the right
// type slice, and that a slug surfacing in two batches ends up with the union
// of its source chunks.
func TestMergeCitationsIntoItems_AddsNewSlugsAndUnionsChunksAcrossBatches(t *testing.T) {
	entities := []extractedItem{
		{Name: "Known", Slug: "entity/known"},
	}
	concepts := []extractedItem{}

	newSlugs := []newSlugFromCitation{
		{
			Type:         "entity",
			Name:         "Fresh Entity",
			Slug:         "entity/fresh",
			Description:  "desc",
			Details:      "details",
			SourceChunks: []string{"c001", "c002"},
		},
		{
			// Same slug as above, appears in another batch — must union.
			Type:         "entity",
			Name:         "Fresh Entity",
			Slug:         "entity/fresh",
			SourceChunks: []string{"c002", "c003"},
		},
		{
			Type:         "concept",
			Name:         "New Concept",
			Slug:         "concept/new-concept",
			SourceChunks: []string{"c010"},
		},
		{
			// Duplicate of an existing candidate — should NOT produce a
			// duplicate entry (Known already exists in `entities`).
			Type:         "entity",
			Name:         "Known",
			Slug:         "entity/known",
			SourceChunks: []string{"c020"},
		},
	}

	gotE, gotC, _ := mergeCitationsIntoItems(entities, concepts, nil, newSlugs)

	if len(gotE) != 2 {
		t.Fatalf("expected 2 entities, got %d (%+v)", len(gotE), gotE)
	}
	if len(gotC) != 1 {
		t.Fatalf("expected 1 concept, got %d (%+v)", len(gotC), gotC)
	}
	fresh := findBySlug(gotE, "entity/fresh")
	if fresh == nil {
		t.Fatalf("entity/fresh missing")
	}
	sort.Strings(fresh.SourceChunks)
	if !equalStrings(fresh.SourceChunks, []string{"c001", "c002", "c003"}) {
		t.Errorf("entity/fresh source_chunks = %v, want union [c001 c002 c003]", fresh.SourceChunks)
	}
	newC := findBySlug(gotC, "concept/new-concept")
	if newC == nil || !equalStrings(newC.SourceChunks, []string{"c010"}) {
		t.Errorf("concept/new-concept missing or wrong chunks: %+v", newC)
	}
}

// TestSplitChunksIntoCitationBatches_RespectsBudgetAndOrder verifies that the
// batcher never puts too many runes in one batch, preserves document order,
// and that an oversized chunk gets its own batch.
func TestSplitChunksIntoCitationBatches_RespectsBudgetAndOrder(t *testing.T) {
	// Each small chunk is 5k runes → 3 of them should fit in one batch
	// (15k > 12k limit would spill to a second batch).
	mk := func(idx int, runes int, id string) *types.Chunk {
		return &types.Chunk{
			ID:         id,
			ChunkIndex: idx,
			Content:    repeatRune('a', runes),
			ChunkType:  types.ChunkTypeText,
		}
	}
	chunks := []*types.Chunk{
		mk(0, 5000, "id-0"),
		mk(1, 5000, "id-1"),
		mk(2, 5000, "id-2"), // this should start a new batch (15k > 12k)
		// An oversized chunk gets a dedicated batch.
		mk(3, 20000, "id-big"),
		mk(4, 1000, "id-small"),
	}
	batches := splitChunksIntoCitationBatches(chunks)
	if len(batches) < 3 {
		t.Fatalf("expected at least 3 batches, got %d", len(batches))
	}
	// All input IDs should show up in some batch, exactly once, in order.
	seen := []string{}
	for _, b := range batches {
		for _, c := range b.chunks {
			seen = append(seen, c.ID)
		}
	}
	wantOrder := []string{"id-0", "id-1", "id-2", "id-big", "id-small"}
	if !equalStrings(seen, wantOrder) {
		t.Errorf("batch order = %v, want %v", seen, wantOrder)
	}

	// Verify alias → id map is populated per batch with unique aliases.
	for bi, b := range batches {
		if len(b.aliasToID) != len(b.chunks) {
			t.Errorf("batch %d alias count %d != chunk count %d", bi, len(b.aliasToID), len(b.chunks))
		}
	}
}

// --- helpers ---

func findBySlug(items []extractedItem, slug string) *extractedItem {
	for i := range items {
		if items[i].Slug == slug {
			return &items[i]
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func repeatRune(r rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = r
	}
	return string(out)
}
