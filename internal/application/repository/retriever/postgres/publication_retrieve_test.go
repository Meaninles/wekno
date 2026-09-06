package postgres

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/retrievalfence"
	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/require"
)

func TestRepairPostgresPublicationDoesNotLoseTopKDuringReindex(t *testing.T) {
	db := testsupport.Postgres(t, &types.Chunk{}, &types.Knowledge{})
	conn, err := db.DB()
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	// The isolated schema owns every table; public only resolves the installed
	// vector and ParadeDB extension types/operators.
	require.NoError(t, db.Exec(`SET search_path = "`+schema+`", public`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE embeddings (
	 id bigserial PRIMARY KEY, source_id text, source_type integer,
	 chunk_id text, knowledge_id text, knowledge_base_id text, tag_id text,
	 content text, dimension integer, embedding halfvec, is_enabled boolean
	)`).Error)
	require.NoError(t, db.Exec(`CREATE INDEX publication_fixture_bm25 ON embeddings USING bm25 (id, content) WITH (key_field='id')`).Error)
	require.NoError(t, db.Create(&types.Knowledge{ID: "doc", TenantID: 7, KnowledgeBaseID: "kb", EnableStatus: "enabled", PublishedGeneration: "old", ProcessingGeneration: "new"}).Error)
	for i, id := range []string{"draft-1", "draft-2", "live-1", "live-2", "foreign"} {
		generation, kbID := "old", "kb"
		if i < 2 {
			generation = "new"
		}
		if id == "foreign" {
			kbID = "other-kb"
		}
		require.NoError(t, db.Create(&types.Chunk{ID: id, TenantID: 7, KnowledgeID: "doc", KnowledgeBaseID: kbID, IsEnabled: true, ProcessingGeneration: generation}).Error)
		require.NoError(t, db.Table("embeddings").Create(map[string]any{"source_id": id, "source_type": 1, "chunk_id": id, "knowledge_id": "doc", "knowledge_base_id": kbID,
			"content": "publication", "dimension": 3, "embedding": pgvector.NewHalfVector([]float32{1, float32(i) * .1, 0}), "is_enabled": true}).Error)
	}
	repo := &pgRepository{db: db}
	loadChunks := func(ctx context.Context, ids []string) ([]*types.Chunk, error) {
		var rows []*types.Chunk
		err := db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error
		return rows, err
	}
	loadKnowledge := func(ctx context.Context, tenant uint64, ids []string) ([]*types.Knowledge, error) {
		var rows []*types.Knowledge
		err := db.WithContext(ctx).Where("tenant_id = ? AND id IN ?", tenant, ids).Find(&rows).Error
		return rows, err
	}
	for _, kind := range []types.RetrieverType{types.VectorRetrieverType, types.KeywordsRetrieverType} {
		param := types.RetrieveParams{RetrieverType: kind, KnowledgeBaseIDs: []string{"kb"}, Query: "publication", Embedding: []float32{1, 0, 0}, TopK: 2}
		fetch := func(ctx context.Context, params []types.RetrieveParams) ([]*types.RetrieveResult, error) {
			return repo.Retrieve(ctx, params[0])
		}
		results, err := retrievalfence.Retrieve(context.Background(), []types.RetrieveParams{param}, []retrievalfence.Scope{{TenantID: 7, KnowledgeBaseID: "kb"}}, fetch, loadChunks, loadKnowledge)
		require.NoError(t, err)
		require.Len(t, results, 1)
		var ids []string
		for _, hit := range results[0].Results {
			ids = append(ids, hit.ChunkID)
		}
		require.ElementsMatch(t, []string{"live-1", "live-2"}, ids)
		param.ExcludeKnowledgeIDs = []string{"doc"}
		empty, err := fetch(context.Background(), []types.RetrieveParams{param})
		require.NoError(t, err)
		for _, result := range empty {
			require.Empty(t, result.Results)
		}
	}
}
