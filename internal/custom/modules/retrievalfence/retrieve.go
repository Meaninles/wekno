package retrievalfence

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
)

type Retriever func(context.Context, []types.RetrieveParams) ([]*types.RetrieveResult, error)

// Retrieve fills each candidate budget with visible evidence. Rejected chunk
// identities are excluded at the store on the next request, so unpublished
// generations cannot consume the entire TopK. Query, score and scope remain
// unchanged. All attempts share the caller's deadline.
func Retrieve(ctx context.Context, params []types.RetrieveParams, scopes []Scope,
	fetch Retriever, loadChunks ChunkLoader, loadKnowledge KnowledgeLoader,
) ([]*types.RetrieveResult, error) {
	pending := append([]types.RetrieveParams(nil), params...)
	var complete []*types.RetrieveResult
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := fetch(ctx, pending)
		if err != nil {
			return nil, err
		}
		visible, err := Filter(ctx, raw, scopes, loadChunks, loadKnowledge)
		if err != nil {
			return nil, err
		}
		var next []types.RetrieveParams
		for _, param := range pending {
			var rawCount int
			var selected []*types.RetrieveResult
			allowed := make(map[string]bool)
			for _, result := range visible {
				if result.RetrieverType == param.RetrieverType {
					selected = append(selected, result)
					for _, hit := range result.Results {
						allowed[hit.ChunkID] = true
					}
				}
			}
			excluded := make(map[string]bool, len(param.ExcludeChunkIDs))
			for _, id := range param.ExcludeChunkIDs {
				excluded[id] = true
			}
			var rejected []string
			for _, result := range raw {
				if result == nil || result.RetrieverType != param.RetrieverType {
					continue
				}
				if result.Error != nil {
					return nil, result.Error
				}
				rawCount += len(result.Results)
				for _, hit := range result.Results {
					if hit == nil || hit.ChunkID == "" {
						return nil, fmt.Errorf("retrieval returned a candidate without a chunk identity")
					}
					if excluded[hit.ChunkID] {
						return nil, fmt.Errorf("retrieval backend did not honor excluded chunk identities")
					}
					if !allowed[hit.ChunkID] {
						rejected = append(rejected, hit.ChunkID)
					}
				}
			}
			if len(rejected) == 0 || rawCount < param.TopK {
				complete = append(complete, selected...)
				continue
			}
			param.ExcludeChunkIDs = append([]string(nil), param.ExcludeChunkIDs...)
			for _, id := range rejected {
				if !excluded[id] {
					param.ExcludeChunkIDs = append(param.ExcludeChunkIDs, id)
					excluded[id] = true
				}
			}
			next = append(next, param)
		}
		pending = next
	}
	// A generation may have changed while another channel was being refilled.
	return Filter(ctx, complete, scopes, loadChunks, loadKnowledge)
}
