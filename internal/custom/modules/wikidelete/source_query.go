package wikidelete

import (
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"strings"
)

func escapeSourcePattern(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}
func SourceRefQuery(
	query *gorm.DB,
	kbID string,
	sourceKnowledgeID string,
) (*gorm.DB, error) {
	if query == nil {
		return nil, errors.New("wiki page source-ref query is nil")
	}
	if query.Dialector != nil && query.Dialector.Name() == "sqlite" {
		prefixPattern := escapeSourcePattern(sourceKnowledgeID) + "|%"
		return query.Where(
			`knowledge_base_id = ? AND EXISTS (
				SELECT 1
				  FROM json_each(
					CASE WHEN json_valid(wiki_pages.source_refs)
					     THEN wiki_pages.source_refs ELSE '[]' END
				  ) AS source_ref
				 WHERE CAST(source_ref.value AS TEXT) = ?
				    OR CAST(source_ref.value AS TEXT) LIKE ? ESCAPE '\'
			)`,
			kbID,
			sourceKnowledgeID,
			prefixPattern,
		), nil
	}

	needle, err := json.Marshal([]string{sourceKnowledgeID})
	if err != nil {
		return nil, fmt.Errorf("marshal source ref needle: %w", err)
	}
	prefix, err := json.Marshal(sourceKnowledgeID + "|")
	if err != nil {
		return nil, fmt.Errorf("marshal source ref prefix: %w", err)
	}
	prefixStr := string(prefix)
	if len(prefixStr) >= 2 && prefixStr[len(prefixStr)-1] == '"' {
		prefixStr = prefixStr[:len(prefixStr)-1]
	}
	likePattern := "%" + escapeSourcePattern(prefixStr) + "%"

	return query.Where(
		"knowledge_base_id = ? AND (source_refs::jsonb @> ?::jsonb OR source_refs::text LIKE ?)",
		kbID,
		string(needle),
		likePattern,
	), nil
}
