package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
)

// RetrieveEngine defines the retrieve engine interface
type RetrieveEngine interface {
	// EngineType gets the retrieve engine type
	EngineType() types.RetrieverEngineType

	// Retrieve executes the retrieve
	Retrieve(ctx context.Context, params types.RetrieveParams) ([]*types.RetrieveResult, error)

	// Support gets the supported retrieve types
	Support() []types.RetrieverType
}

// RetrieveEngineRepository defines the retrieve engine repository interface
type RetrieveEngineRepository interface {
	// Save saves the index info
	Save(ctx context.Context, indexInfo *types.IndexInfo, params map[string]any) error

	// BatchSave saves the index info list
	BatchSave(ctx context.Context, indexInfoList []*types.IndexInfo, params map[string]any) error

	// EstimateStorageSize estimates the storage size
	EstimateStorageSize(ctx context.Context, indexInfoList []*types.IndexInfo, params map[string]any) int64

	// DeleteByChunkIDList deletes the index info by chunk id list
	DeleteByChunkIDList(ctx context.Context, indexIDList []string, dimension int, knowledgeType string) error
	// DeleteBySourceIDList deletes the index info by source id list
	DeleteBySourceIDList(ctx context.Context, sourceIDList []string, dimension int, knowledgeType string) error
	// 复制索引数据
	// sourceKnowledgeBaseID: 源知识库ID
	// sourceToTargetChunkIDMap: 源分块ID到目标分块ID的映射关系
	// targetKnowledgeBaseID: 目标知识库ID
	// params: 额外参数，如向量表示等
	CopyIndices(
		ctx context.Context,
		sourceKnowledgeBaseID string,
		sourceToTargetKBIDMap map[string]string,
		sourceToTargetChunkIDMap map[string]string,
		targetKnowledgeBaseID string,
		dimension int,
		knowledgeType string,
	) error

	// DeleteByKnowledgeIDList deletes the index info by knowledge id list
	DeleteByKnowledgeIDList(ctx context.Context, knowledgeIDList []string, dimension int, knowledgeType string) error

	// BatchUpdateChunkEnabledStatus updates the enabled status of chunks in batch
	// chunkStatusMap: map of chunk ID to enabled status (true = enabled, false = disabled)
	BatchUpdateChunkEnabledStatus(ctx context.Context, chunkStatusMap map[string]bool) error

	// BatchUpdateChunkTagID updates the tag ID of chunks in batch
	// chunkTagMap: map of chunk ID to tag ID (empty string means no tag)
	BatchUpdateChunkTagID(ctx context.Context, chunkTagMap map[string]string) error

	// RetrieveEngine retrieves the engine
	RetrieveEngine
}

// ScopedKnowledgeIndexDeleter deletes indices only when both the knowledge
// base and knowledge IDs match.  This is deliberately kept as an optional,
// narrow capability rather than folded into RetrieveEngineRepository: callers
// that move data between knowledge bases must fail closed when a backend has
// not implemented the scoped primitive, while ordinary delete paths retain
// their existing API.
//
// A knowledge ID is stable across a reuse-vectors move.  Consequently an
// unscoped DeleteByKnowledgeIDList after CopyIndices would delete both the
// source rows and the newly-created target rows on backends that share one
// collection/table.  This capability is the safe primitive used by the move
// transaction below.
type ScopedKnowledgeIndexDeleter interface {
	DeleteByKnowledgeBaseAndKnowledgeIDList(
		ctx context.Context,
		knowledgeBaseID string,
		knowledgeIDList []string,
		dimension int,
		knowledgeType string,
	) error
}

// KnowledgeIndexInPlaceMover is the repository capability required by
// reuse_vectors. The implementation must update the existing index rows in
// place, atomically within that backend, while matching both the source KB and
// knowledge ID. A CopyIndices-based implementation is not valid: source_id is
// stable during a move and several backends enforce uniqueness on it, causing
// the copy to be skipped before the source rows are deleted.
//
// Repositories that cannot guarantee this contract must not implement the
// interface. The service preflights the capability before claiming the
// knowledge row and directs callers to reparse mode instead.
type KnowledgeIndexInPlaceMover interface {
	MoveKnowledgeIndicesInPlace(
		ctx context.Context,
		sourceKnowledgeBaseID string,
		targetKnowledgeBaseID string,
		knowledgeID string,
		chunkIDs []string,
		dimension int,
		knowledgeType string,
	) error
}

// KnowledgeIndexInPlaceMoveError may be returned by an in-place mover after a
// failed write. SourceIntact is true only when a read-back proved that all rows
// remain exclusively in the source KB. Missing/unknown implementations are
// treated as an uncertain commit and force recovery rather than a lifecycle
// rollback.
type KnowledgeIndexInPlaceMoveError interface {
	error
	SourceIntact() bool
}

// KnowledgeIndexMover is the fail-closed service-level capability used by a
// reuse-vectors knowledge move. Implementations delegate only to a repository
// KnowledgeIndexInPlaceMover; ordinary CopyIndices is never an acceptable
// implementation of this contract.
type KnowledgeIndexMover interface {
	SupportsKnowledgeIndexMove() bool
	MoveKnowledgeIndices(
		ctx context.Context,
		sourceKnowledgeBaseID string,
		targetKnowledgeBaseID string,
		knowledgeID string,
		chunkIDs []string,
		dimension int,
		knowledgeType string,
	) error
}

// RetrieveEngineRegistry defines the retrieve engine registry interface
type RetrieveEngineRegistry interface {
	// Register registers the retrieve engine service
	Register(indexService RetrieveEngineService) error
	// GetRetrieveEngineService gets the retrieve engine service
	GetRetrieveEngineService(engineType types.RetrieverEngineType) (RetrieveEngineService, error)
	// GetAllRetrieveEngineServices gets all retrieve engine services
	GetAllRetrieveEngineServices() []RetrieveEngineService

	// GetByStoreID returns the engine service registered for a specific DB store ID.
	//
	// IMPORTANT: This method does NOT verify tenant ownership of the returned
	// store. Callers MUST use the CreateRetrieveEngineForKB /
	// CreateRetrieveEngineFromPayload factory functions in the retriever package
	// rather than calling this directly. The factories wrap GetByStoreID with
	// tenant ownership verification (defense-in-depth against cross-tenant IDOR).
	GetByStoreID(storeID string) (RetrieveEngineService, error)
}

// RetrieveEngineService defines the retrieve engine service interface
type RetrieveEngineService interface {
	// Index indexes the index info
	Index(ctx context.Context,
		embedder embedding.Embedder,
		indexInfo *types.IndexInfo,
		retrieverTypes []types.RetrieverType,
	) error

	// BatchIndex indexes the index info list
	BatchIndex(ctx context.Context,
		embedder embedding.Embedder,
		indexInfoList []*types.IndexInfo,
		retrieverTypes []types.RetrieverType,
	) error

	// EstimateStorageSize estimates the storage size
	EstimateStorageSize(ctx context.Context,
		embedder embedding.Embedder,
		indexInfoList []*types.IndexInfo,
		retrieverTypes []types.RetrieverType,
	) int64
	// CopyIndices 从源知识库复制索引到目标知识库，免去重新计算嵌入向量的开销
	// sourceKnowledgeBaseID: 源知识库ID
	// sourceToTargetChunkIDMap: 源分块ID到目标分块ID的映射关系，key为源分块ID，value为目标分块ID
	// targetKnowledgeBaseID: 目标知识库ID
	CopyIndices(
		ctx context.Context,
		sourceKnowledgeBaseID string,
		sourceToTargetKBIDMap map[string]string,
		sourceToTargetChunkIDMap map[string]string,
		targetKnowledgeBaseID string,
		dimension int,
		knowledgeType string,
	) error

	// DeleteByChunkIDList deletes the index info by chunk id list
	DeleteByChunkIDList(ctx context.Context, indexIDList []string, dimension int, knowledgeType string) error

	// DeleteBySourceIDList deletes the index info by source id list
	DeleteBySourceIDList(ctx context.Context, sourceIDList []string, dimension int, knowledgeType string) error

	// DeleteByKnowledgeIDList deletes the index info by knowledge id list
	DeleteByKnowledgeIDList(ctx context.Context, knowledgeIDList []string, dimension int, knowledgeType string) error

	// BatchUpdateChunkEnabledStatus updates the enabled status of chunks in batch
	// chunkStatusMap: map of chunk ID to enabled status (true = enabled, false = disabled)
	BatchUpdateChunkEnabledStatus(ctx context.Context, chunkStatusMap map[string]bool) error

	// BatchUpdateChunkTagID updates the tag ID of chunks in batch
	// chunkTagMap: map of chunk ID to tag ID (empty string means no tag)
	BatchUpdateChunkTagID(ctx context.Context, chunkTagMap map[string]string) error

	// RetrieveEngine retrieves the engine
	RetrieveEngine
}
