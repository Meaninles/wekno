package service

import (
	"context"
	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/types"
)

// CleanupRetiredIndexes supplies native model/store adapters to the durable
// custom lifecycle. The maintenance leader owns scheduling and cancellation.
func (s *knowledgeService) CleanupRetiredIndexes(ctx context.Context) error {
	return s.auxObjects.DrainIndexRetirements(ctx, func(ctx context.Context, tenantID uint64, receipt knowledgeaux.IndexRetirement) error {
		tenant, err := s.tenantRepo.GetTenantByID(ctx, tenantID)
		if err != nil {
			return err
		}
		ctx = context.WithValue(ctx, types.TenantIDContextKey, tenantID)
		ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenant)
		if err := s.graphEngine.DelGraph(ctx, []types.NameSpace{{KnowledgeBase: receipt.KnowledgeBaseID, Knowledge: receipt.KnowledgeID, Generation: &receipt.Generation}}); err != nil {
			return err
		}
		if err := s.auxObjects.CleanupRetiredGeneration(ctx, tenantID, receipt); err != nil {
			return err
		}
		if receipt.EmbeddingModelID == "" || len(receipt.ChunkIDs) == 0 {
			return nil
		}
		model, err := s.modelService.GetEmbeddingModel(ctx, receipt.EmbeddingModelID)
		if err != nil {
			return err
		}
		engine, err := retriever.CreateRetrieveEngineForKB(ctx, s.retrieveEngine, s.ownership, tenantID, receipt.VectorStoreID)
		if err != nil {
			return err
		}
		return engine.DeleteByChunkIDList(ctx, receipt.ChunkIDs, model.GetDimensions(), receipt.KnowledgeType)
	})
}
