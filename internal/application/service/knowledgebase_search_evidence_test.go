package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestBuildChunkIndexCapturesRetrievalOriginWithoutChangingExistingScoreSemantics(t *testing.T) {
	service := &knowledgeBaseService{}
	index := service.buildChunkIndex([]*types.IndexWithScore{
		{ChunkID: "chunk-1", KnowledgeID: "doc-1", SourceID: "chunk-1", Content: "正文命中", Score: 0.92},
		{ChunkID: "chunk-1", KnowledgeID: "doc-1", SourceID: "question-1", Content: "生成的问题", Score: 0.81},
	})

	// Existing retrieval assembly uses the last occurrence for score/match text.
	// Citation provenance records the matching source ID but does not reorder or
	// rescore retrieval candidates.
	if len(index.chunkIDs) != 2 || index.scores["chunk-1"] != 0.81 ||
		index.matchedContents["chunk-1"] != "生成的问题" || index.matchedSourceIDs["chunk-1"] != "question-1" {
		t.Fatalf("retrieval semantics changed while adding provenance: %#v", index)
	}
}

func TestRetrievalMatchOriginRecognizesGeneratedQuestionMetadata(t *testing.T) {
	chunk := &types.Chunk{
		ID:        "chunk-1",
		ChunkType: types.ChunkTypeText,
		Metadata:  types.JSON(`{"generated_questions":[{"id":"question-1","question":"流程是什么？"}]}`),
	}
	if got := retrievalMatchOrigin(chunk, "question-1"); got != "generated_question" {
		t.Fatalf("origin = %q, want generated_question", got)
	}
	if got := retrievalMatchOrigin(chunk, "chunk-1"); got != "chunk_body" {
		t.Fatalf("origin = %q, want chunk_body", got)
	}
}
