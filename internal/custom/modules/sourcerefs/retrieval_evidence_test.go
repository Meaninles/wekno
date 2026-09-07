package sourcerefs

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

type exactEvidenceRepoStub struct {
	byID             map[string]*types.Chunk
	childrenByParent map[string][]*types.Chunk
	parentTenantIDs  []uint64
}

func (s *exactEvidenceRepoStub) ListChunksByIDOnly(_ context.Context, ids []string) ([]*types.Chunk, error) {
	out := make([]*types.Chunk, 0, len(ids))
	for _, id := range ids {
		if chunk := s.byID[id]; chunk != nil {
			out = append(out, chunk)
		}
	}
	return out, nil
}

func (s *exactEvidenceRepoStub) ListChunksByParentIDs(
	_ context.Context,
	tenantID uint64,
	parentIDs []string,
) ([]*types.Chunk, error) {
	s.parentTenantIDs = append(s.parentTenantIDs, tenantID)
	var out []*types.Chunk
	for _, parentID := range parentIDs {
		out = append(out, s.childrenByParent[parentID]...)
	}
	return out, nil
}

func TestResolveRetrievalEvidenceNeverUsesGeneratedQuestionAsEvidence(t *testing.T) {
	repo := &exactEvidenceRepoStub{byID: map[string]*types.Chunk{
		"chunk-32": {
			ID: "chunk-32", TenantID: 7, KnowledgeID: "doc-1", ChunkType: types.ChunkTypeText,
			Content: "第三十二条 采购方式包括招标、询比、竞价、谈判、框架协议和单源采购。",
		},
	}}
	merged := []*types.SearchResult{{
		ID: "chunk-32", Content: "第三十二条 采购方式包括招标、询比、竞价、谈判、框架协议和单源采购。",
		MatchedContent: "项目立项决策后的采购流程和要求是什么？", MatchOrigin: "generated_question",
		KnowledgeID: "doc-1", KnowledgeTitle: "采购管理办法.docx", ChunkType: string(types.ChunkTypeText),
		SourceTenantID: 7,
	}}

	refs, err := ResolveRetrievalEvidence(context.Background(), repo, 7, merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(refs))
	}
	if refs[0].EvidenceContent != repo.byID["chunk-32"].Content || refs[0].Content != repo.byID["chunk-32"].Content {
		t.Fatalf("unexpected evidence content: %#v", refs[0])
	}
	if refs[0].MatchedContent != "" || refs[0].MatchOrigin != "" || refs[0].ID != "chunk-32" {
		t.Fatalf("retrieval provenance leaked into exact evidence: %#v", refs[0])
	}
}

func TestResolveRetrievalEvidenceSplitsParentContextIntoExactChildren(t *testing.T) {
	children := []*types.Chunk{
		{ID: "child-3", TenantID: 42, KnowledgeID: "doc-1", ParentChunkID: "parent-1", ChunkType: types.ChunkTypeText, Content: "第三段", StartAt: 20, EndAt: 30, ChunkIndex: 3},
		{ID: "child-1", TenantID: 42, KnowledgeID: "doc-1", ParentChunkID: "parent-1", ChunkType: types.ChunkTypeText, Content: "第一段", StartAt: 0, EndAt: 10, ChunkIndex: 1},
		{ID: "child-2", TenantID: 42, KnowledgeID: "doc-1", ParentChunkID: "parent-1", ChunkType: types.ChunkTypeText, Content: "第二段", StartAt: 10, EndAt: 20, ChunkIndex: 2},
	}
	repo := &exactEvidenceRepoStub{childrenByParent: map[string][]*types.Chunk{"parent-1": children}}
	merged := []*types.SearchResult{{
		ID: "child-2", Content: "第一段第二段第三段", ParentChunkID: "parent-1", SubChunkID: []string{"child-2"},
		KnowledgeID: "doc-1", KnowledgeTitle: "制度.docx", ChunkType: string(types.ChunkTypeText),
		SourceTenantID: 42, ImageInfo: `[{"url":"matched-child.png"}]`,
	}}

	refs, err := ResolveRetrievalEvidence(context.Background(), repo, 7, merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("refs = %d, want all 3 source fragments represented in model-visible parent context", len(refs))
	}
	for i, want := range []string{"child-1", "child-2", "child-3"} {
		if refs[i].ID != want || refs[i].EvidenceContent == "第一段第二段第三段" {
			t.Fatalf("refs[%d] = %#v, want exact child %s", i, refs[i], want)
		}
	}
	if refs[0].ImageInfo != "" || refs[1].ImageInfo != merged[0].ImageInfo {
		t.Fatalf("matched-child image context leaked to a sibling: %#v", refs)
	}
	if len(repo.parentTenantIDs) != 1 || repo.parentTenantIDs[0] != 42 {
		t.Fatalf("shared source tenant was not used: %v", repo.parentTenantIDs)
	}
}

func TestResolveRetrievalEvidencePreservesExactNeighborMembership(t *testing.T) {
	repo := &exactEvidenceRepoStub{byID: map[string]*types.Chunk{
		"chunk-1": {ID: "chunk-1", KnowledgeID: "doc-1", ChunkType: types.ChunkTypeText, Content: "前文", StartAt: 0, EndAt: 10},
		"chunk-2": {ID: "chunk-2", KnowledgeID: "doc-1", ChunkType: types.ChunkTypeText, Content: "命中正文", StartAt: 10, EndAt: 20},
		"chunk-3": {ID: "chunk-3", KnowledgeID: "doc-1", ChunkType: types.ChunkTypeText, Content: "后文", StartAt: 20, EndAt: 30},
	}}
	merged := []*types.SearchResult{{
		ID: "chunk-2", Content: "前文命中正文后文", SubChunkID: []string{"chunk-3", "chunk-1"},
		KnowledgeID: "doc-1", ChunkType: string(types.ChunkTypeText), SourceTenantID: 7,
	}}

	refs, err := ResolveRetrievalEvidence(context.Background(), repo, 7, merged)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"chunk-1", "chunk-2", "chunk-3"} {
		if len(refs) != 3 || refs[i].ID != want {
			t.Fatalf("unexpected exact neighbor order: %#v", refs)
		}
	}
}

func TestResolveRetrievalEvidenceKeepsAuthoritativeFAQAnswer(t *testing.T) {
	faq := &types.SearchResult{
		ID: "faq-1", Content: "Q: 如何申请？\nAnswer:\n- 在系统中提交申请。",
		KnowledgeID: "faq-doc", ChunkType: string(types.ChunkTypeFAQ),
	}
	refs, err := ResolveRetrievalEvidence(context.Background(), nil, 7, []*types.SearchResult{faq})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].EvidenceContent != faq.Content {
		t.Fatalf("FAQ answer was not preserved: %#v", refs)
	}
}

func TestResolveRetrievalEvidenceMapsImageHitToTextFragment(t *testing.T) {
	repo := &exactEvidenceRepoStub{byID: map[string]*types.Chunk{
		"text-1": {ID: "text-1", TenantID: 7, KnowledgeID: "doc-1", ChunkType: types.ChunkTypeText, Content: "图表对应的原文说明。", StartAt: 100, EndAt: 120},
	}}
	image := &types.SearchResult{
		ID: "ocr-1", Content: "OCR 检索文本", ParentChunkID: "text-1",
		KnowledgeID: "doc-1", ChunkType: string(types.ChunkTypeImageOCR), SourceTenantID: 7,
	}
	refs, err := ResolveRetrievalEvidence(context.Background(), repo, 7, []*types.SearchResult{image})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].ID != "text-1" || refs[0].EvidenceContent != "图表对应的原文说明。" {
		t.Fatalf("image hit was not mapped to the readable text fragment: %#v", refs)
	}
}

func TestResolveRetrievalEvidenceMapsGeneratedSummaryToParentText(t *testing.T) {
	repo := &exactEvidenceRepoStub{byID: map[string]*types.Chunk{
		"text-1": {
			ID: "text-1", TenantID: 7, KnowledgeID: "doc-1",
			ChunkType: types.ChunkTypeText, Content: "Sev-2 首次确认时限为十五分钟。",
		},
	}}
	summary := &types.SearchResult{
		ID: "summary-1", Content: "模型生成的文档摘要，不得直接作为证据。",
		ParentChunkID: "text-1", KnowledgeID: "doc-1",
		ChunkType: string(types.ChunkTypeSummary), SourceTenantID: 7,
	}

	refs, err := ResolveRetrievalEvidence(context.Background(), repo, 7, []*types.SearchResult{summary})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].ID != "text-1" || refs[0].EvidenceContent != repo.byID["text-1"].Content {
		t.Fatalf("summary evidence = %#v, want exact parent text", refs)
	}
	if refs[0].Content == summary.Content {
		t.Fatalf("generated summary leaked into evidence: %#v", refs[0])
	}
}

func TestResolveRetrievalEvidenceDirectChildDoesNotExpandUnselectedSiblings(t *testing.T) {
	repo := &exactEvidenceRepoStub{childrenByParent: map[string][]*types.Chunk{"parent": {{ID: "other", Content: "unselected unrelated text", ChunkType: types.ChunkTypeText}}}}
	hit := &types.SearchResult{ID: "selected", KnowledgeID: "doc", ParentChunkID: "parent", ChunkType: string(types.ChunkTypeText), Content: "exact selected text"}
	refs, err := ResolveRetrievalEvidence(context.Background(), repo, 7, []*types.SearchResult{hit})
	if err != nil || len(refs) != 1 || refs[0].ID != "selected" || refs[0].EvidenceContent != hit.Content || len(repo.parentTenantIDs) != 0 {
		t.Fatalf("unexpected expansion: %#v %v", refs, err)
	}
}
