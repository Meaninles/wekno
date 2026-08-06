package dependencycontrol

import (
	"context"
	"errors"

	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type guardedEngine struct {
	interfaces.RetrieveEngineService
	control *Service
}

func WrapPostgresEngine(
	inner interfaces.RetrieveEngineService, control *Service,
) interfaces.RetrieveEngineService {
	if inner == nil || control == nil {
		return inner
	}
	return &guardedEngine{RetrieveEngineService: inner, control: control}
}

func (g *guardedEngine) run(ctx context.Context, operation func() error) error {
	if err := g.control.Before(ctx, CapabilityKeywordIndex, KeywordIndexScope); err != nil {
		return err
	}
	return g.control.Observe(ctx, CapabilityKeywordIndex, KeywordIndexScope, operation())
}

func (g *guardedEngine) Index(
	ctx context.Context, embedder embedding.Embedder, info *types.IndexInfo,
	retrieverTypes []types.RetrieverType,
) error {
	return g.run(ctx, func() error {
		return g.RetrieveEngineService.Index(ctx, embedder, info, retrieverTypes)
	})
}

func (g *guardedEngine) BatchIndex(
	ctx context.Context, embedder embedding.Embedder, infos []*types.IndexInfo,
	retrieverTypes []types.RetrieverType,
) error {
	return g.run(ctx, func() error {
		return g.RetrieveEngineService.BatchIndex(ctx, embedder, infos, retrieverTypes)
	})
}

func (g *guardedEngine) Retrieve(
	ctx context.Context, params types.RetrieveParams,
) ([]*types.RetrieveResult, error) {
	if params.RetrieverType != types.KeywordsRetrieverType {
		return g.RetrieveEngineService.Retrieve(ctx, params)
	}
	if err := g.control.Before(ctx, CapabilityKeywordIndex, KeywordIndexScope); err != nil {
		return nil, err
	}
	results, err := g.RetrieveEngineService.Retrieve(ctx, params)
	if err != nil {
		return nil, g.control.Observe(ctx, CapabilityKeywordIndex, KeywordIndexScope, err)
	}
	return results, nil
}

func (g *guardedEngine) DeleteByChunkIDList(
	ctx context.Context, ids []string, dimension int, knowledgeType string,
) error {
	return g.run(ctx, func() error {
		return g.RetrieveEngineService.DeleteByChunkIDList(ctx, ids, dimension, knowledgeType)
	})
}

func (g *guardedEngine) DeleteBySourceIDList(
	ctx context.Context, ids []string, dimension int, knowledgeType string,
) error {
	return g.run(ctx, func() error {
		return g.RetrieveEngineService.DeleteBySourceIDList(ctx, ids, dimension, knowledgeType)
	})
}

func (g *guardedEngine) DeleteByKnowledgeIDList(
	ctx context.Context, ids []string, dimension int, knowledgeType string,
) error {
	return g.run(ctx, func() error {
		return g.RetrieveEngineService.DeleteByKnowledgeIDList(ctx, ids, dimension, knowledgeType)
	})
}

func (g *guardedEngine) CopyIndices(
	ctx context.Context, sourceKB string, knowledgeMap map[string]string,
	chunkMap map[string]string, targetKB string, dimension int, knowledgeType string,
) error {
	return g.run(ctx, func() error {
		return g.RetrieveEngineService.CopyIndices(
			ctx, sourceKB, knowledgeMap, chunkMap, targetKB, dimension, knowledgeType,
		)
	})
}

func (g *guardedEngine) BatchUpdateChunkEnabledStatus(
	ctx context.Context, values map[string]bool,
) error {
	return g.run(ctx, func() error {
		return g.RetrieveEngineService.BatchUpdateChunkEnabledStatus(ctx, values)
	})
}

func (g *guardedEngine) BatchUpdateChunkTagID(
	ctx context.Context, values map[string]string,
) error {
	return g.run(ctx, func() error {
		return g.RetrieveEngineService.BatchUpdateChunkTagID(ctx, values)
	})
}

func (g *guardedEngine) SupportsKnowledgeIndexMove() bool {
	mover, ok := g.RetrieveEngineService.(interfaces.KnowledgeIndexMover)
	return ok && mover.SupportsKnowledgeIndexMove()
}

func (g *guardedEngine) MoveKnowledgeIndices(
	ctx context.Context, sourceKB, targetKB, knowledgeID string,
	chunkIDs []string, dimension int, knowledgeType string,
) error {
	mover, ok := g.RetrieveEngineService.(interfaces.KnowledgeIndexMover)
	if !ok {
		return errors.New("guarded postgres engine does not support in-place moves")
	}
	return g.run(ctx, func() error {
		return mover.MoveKnowledgeIndices(
			ctx, sourceKB, targetKB, knowledgeID, chunkIDs, dimension, knowledgeType,
		)
	})
}
