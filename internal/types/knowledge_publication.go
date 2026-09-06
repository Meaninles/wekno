package types

import "context"

type knowledgePublicationKey struct{}
type publishedChunkReadKey struct{}

func WithPublishedChunks(ctx context.Context) context.Context {
	return context.WithValue(ctx, publishedChunkReadKey{}, true)
}
func PublishedChunksOnly(ctx context.Context) bool {
	enabled, _ := ctx.Value(publishedChunkReadKey{}).(bool)
	return enabled
}

func WithStagedKnowledge(ctx context.Context) context.Context {
	return context.WithValue(ctx, knowledgePublicationKey{}, true)
}
func KnowledgePublicationFromContext(ctx context.Context) string {
	if staged, _ := ctx.Value(knowledgePublicationKey{}).(bool); staged {
		return "staged"
	}
	return "published"
}
func (k *Knowledge) IsPublished() bool {
	return k != nil && k.PublicationState != "staged" && k.PublicationState != "retired"
}

// IsPublishedChunk reads the committed evidence version. A newer processing
// attempt (including a failed one) must not hide the previous readable index.
func (k *Knowledge) IsPublishedChunk(chunk *Chunk) bool {
	if !k.IsPublished() || k.DeletedAt.Valid || k.EnableStatus != "enabled" ||
		chunk == nil || !chunk.IsEnabled || chunk.DeletedAt.Valid ||
		chunk.TenantID != k.TenantID || chunk.KnowledgeID != k.ID || chunk.KnowledgeBaseID != k.KnowledgeBaseID {
		return false
	}
	if k.Type == KnowledgeTypeFAQ {
		return k.CoreStatus == CoreStatusReady
	}
	return k.PublishedGeneration != "" && chunk.ProcessingGeneration == k.PublishedGeneration
}
