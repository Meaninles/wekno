package knowledgesearch

import "github.com/Tencent/WeKnora/internal/types"

// Result is the compact representation returned by the knowledge-name search
// endpoint. Search suggestions only need identity and display fields; excluding
// descriptions, metadata and storage details keeps every incremental page small.
type Result struct {
	ID                string `json:"id"`
	KnowledgeBaseID   string `json:"knowledge_base_id"`
	Type              string `json:"type"`
	Title             string `json:"title"`
	FileName          string `json:"file_name"`
	FileType          string `json:"file_type"`
	KnowledgeBaseName string `json:"knowledge_base_name"`
}

// Results converts repository rows to the compact public search payload.
func Results(items []*types.Knowledge) []Result {
	results := make([]Result, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		results = append(results, Result{
			ID:                item.ID,
			KnowledgeBaseID:   item.KnowledgeBaseID,
			Type:              item.Type,
			Title:             item.Title,
			FileName:          item.FileName,
			FileType:          item.FileType,
			KnowledgeBaseName: item.KnowledgeBaseName,
		})
	}
	return results
}
