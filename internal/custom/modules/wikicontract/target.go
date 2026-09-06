package wikicontract

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// Target is the identity returned by a page read. A slug alone is not a
// globally unique address, and reading a fresh version cannot substitute for
// the version on which a caller based an edit.
type Target struct {
	KnowledgeBaseID string `json:"knowledge_base_id"`
	PageID          string `json:"page_id,omitempty"`
	ExpectedVersion *int   `json:"expected_version,omitempty"`
}

func (t Target) KnowledgeBase(allowed []string) (string, error) {
	id := strings.TrimSpace(t.KnowledgeBaseID)
	if id == "" && len(allowed) == 1 {
		id = allowed[0]
	}
	if id == "" || !slices.Contains(allowed, id) {
		return "", fmt.Errorf("knowledge_base_id must identify one of the allowed knowledge bases: %v", allowed)
	}
	return id, nil
}

func (t Target) ValidatePage(page *types.WikiPage, write bool) error {
	if page == nil {
		return fmt.Errorf("page does not exist")
	}
	if t.PageID != "" && t.PageID != page.ID {
		return fmt.Errorf("page identity changed: current page_id=%s", page.ID)
	}
	if write && (t.PageID == "" || t.ExpectedVersion == nil) {
		return fmt.Errorf("page_id and expected_version from a page read are required for an existing page")
	}
	if t.ExpectedVersion != nil && *t.ExpectedVersion != page.Version {
		return fmt.Errorf("page version conflict: page_id=%s current_version=%d", page.ID, page.Version)
	}
	return nil
}

// TargetSchema augments the single production schema consumed by both native
// tools and SDK adapters. Conditional existence checks belong to execution.
func TargetSchema(raw json.RawMessage, write bool) json.RawMessage {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		panic(err)
	}
	props := schema["properties"].(map[string]any)
	props["knowledge_base_id"] = map[string]any{"type": "string", "description": "Exact target knowledge base; required when more than one is available."}
	props["page_id"] = map[string]any{"type": "string", "description": "Stable page ID returned by wiki_read_page."}
	if write {
		props["expected_version"] = map[string]any{"type": "integer", "minimum": 0, "description": "Version returned by wiki_read_page. Required for an existing page; 0 means create only."}
	}
	schema["additionalProperties"] = false
	result, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return result
}

type IssueLister interface {
	ListIssues(context.Context, string, string, string) ([]*types.WikiPageIssue, error)
}

// RequireIssue prevents an issue ID from escaping the selected KB scope.
func RequireIssue(ctx context.Context, service IssueLister, kbID, issueID string) error {
	issues, err := service.ListIssues(ctx, kbID, "", "")
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.ID == issueID && issue.KnowledgeBaseID == kbID {
			return nil
		}
	}
	return fmt.Errorf("issue is outside the selected knowledge base or no longer exists")
}
