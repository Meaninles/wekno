package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type generationChunkServiceStub struct {
	interfaces.ChunkService
	chunks      map[string]*types.Chunk
	createCalls int
	updateCalls int
}

func (s *generationChunkServiceStub) GetChunkByID(_ context.Context, id string) (*types.Chunk, error) {
	chunk, ok := s.chunks[id]
	if !ok {
		return nil, errors.New("not found")
	}
	copyChunk := *chunk
	return &copyChunk, nil
}

func (s *generationChunkServiceStub) CreateChunks(_ context.Context, chunks []*types.Chunk) error {
	s.createCalls++
	if s.chunks == nil {
		s.chunks = make(map[string]*types.Chunk)
	}
	for _, chunk := range chunks {
		if _, exists := s.chunks[chunk.ID]; exists {
			return errors.New("duplicate")
		}
		copyChunk := *chunk
		copyChunk.SeqID = 99
		s.chunks[chunk.ID] = &copyChunk
	}
	return nil
}

func (s *generationChunkServiceStub) UpdateChunks(_ context.Context, chunks []*types.Chunk) error {
	s.updateCalls++
	for _, chunk := range chunks {
		copyChunk := *chunk
		s.chunks[chunk.ID] = &copyChunk
	}
	return nil
}

func TestUpsertGenerationChunksUpdatesStableRetryArtifact(t *testing.T) {
	stub := &generationChunkServiceStub{}
	first := &types.Chunk{
		ID: "stable-id", TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1",
		ChunkType: types.ChunkTypeImageOCR, Content: "first",
	}
	if err := upsertGenerationChunks(context.Background(), stub, []*types.Chunk{first}); err != nil {
		t.Fatal(err)
	}
	retry := &types.Chunk{
		ID: "stable-id", TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1",
		ChunkType: types.ChunkTypeImageOCR, Content: "retry",
	}
	if err := upsertGenerationChunks(context.Background(), stub, []*types.Chunk{retry}); err != nil {
		t.Fatal(err)
	}
	if stub.createCalls != 1 || stub.updateCalls != 1 {
		t.Fatalf("writes = create:%d update:%d, want 1/1", stub.createCalls, stub.updateCalls)
	}
	if got := stub.chunks["stable-id"]; got.Content != "retry" || got.SeqID != 99 {
		t.Fatalf("retry artifact = content:%q seq:%d", got.Content, got.SeqID)
	}
}

func TestUpsertGenerationChunksRejectsStableIDIdentityCollision(t *testing.T) {
	stub := &generationChunkServiceStub{chunks: map[string]*types.Chunk{
		"stable-id": {
			ID: "stable-id", TenantID: 99, KnowledgeID: "foreign", KnowledgeBaseID: "kb-other",
			ChunkType: types.ChunkTypeImageOCR,
		},
	}}
	err := upsertGenerationChunks(context.Background(), stub, []*types.Chunk{{
		ID: "stable-id", TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1",
		ChunkType: types.ChunkTypeImageOCR,
	}})
	if err == nil {
		t.Fatal("identity collision error = nil")
	}
}
