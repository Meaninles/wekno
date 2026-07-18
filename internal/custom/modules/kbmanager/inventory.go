package kbmanager

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

type ListDocumentsRequest struct {
	KnowledgeBaseID string `json:"knowledge_base_id,omitempty" jsonschema:"optional KB ID; when omitted list every document in the effective current-turn scope"`
	Query           string `json:"query,omitempty" jsonschema:"optional case-insensitive filename/title filter"`
	Page            int    `json:"page,omitempty" jsonschema:"1-based page, default 1"`
	PageSize        int    `json:"page_size,omitempty" jsonschema:"1-100, default 20"`
}

type DocumentView struct {
	ID                string            `json:"id"`
	KnowledgeBaseID   string            `json:"knowledge_base_id"`
	KnowledgeBaseName string            `json:"knowledge_base_name,omitempty"`
	Title             string            `json:"title"`
	FileName          string            `json:"file_name"`
	FileType          string            `json:"file_type"`
	FileSize          int64             `json:"file_size"`
	FileHash          string            `json:"file_hash"`
	ParseStatus       string            `json:"parse_status"`
	PendingSubtasks   int               `json:"pending_subtasks"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Tags              []TagView         `json:"tags,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type TagView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type DocumentInventory struct {
	Total     int64          `json:"total"`
	Page      int            `json:"page"`
	PageSize  int            `json:"page_size"`
	Documents []DocumentView `json:"documents"`
}

func (s *Service) ListDocuments(ctx context.Context, scope ToolScope, input ListDocumentsRequest) (*DocumentInventory, error) {
	if scope.Runtime == nil {
		return nil, fmt.Errorf("knowledge management runtime scope is unavailable")
	}
	requestedKB := strings.TrimSpace(input.KnowledgeBaseID)
	if requestedKB != "" && !scope.Runtime.HasWholeKnowledgeBase(requestedKB) {
		parentPresent := false
		for _, kbID := range scope.Runtime.Documents {
			if kbID == requestedKB {
				parentPresent = true
				break
			}
		}
		if !parentPresent {
			return nil, fmt.Errorf("knowledge base is outside the current turn scope")
		}
	}

	wholeKBs := compactUnique(scope.Runtime.WholeKnowledgeBaseIDs)
	documentIDs := sortedKeys(scope.Runtime.Documents)
	if requestedKB != "" {
		if scope.Runtime.HasWholeKnowledgeBase(requestedKB) {
			wholeKBs = []string{requestedKB}
			documentIDs = nil
		} else {
			wholeKBs = nil
			filtered := make([]string, 0, len(documentIDs))
			for _, id := range documentIDs {
				if scope.Runtime.Documents[id] == requestedKB {
					filtered = append(filtered, id)
				}
			}
			documentIDs = filtered
		}
	}
	if len(wholeKBs) == 0 && len(documentIDs) == 0 {
		if scope.Runtime.ReadOnlyTagScope {
			return nil, fmt.Errorf("the current turn selected tags only; tag scope is read-only and grants no document inventory mutation scope")
		}
		return &DocumentInventory{Documents: []DocumentView{}}, nil
	}

	page := input.Page
	if page < 1 {
		page = 1
	}
	pageSize := input.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	query := s.db.WithContext(ctx).Model(&types.Knowledge{})
	switch {
	case len(wholeKBs) > 0 && len(documentIDs) > 0:
		query = query.Where("(knowledge_base_id IN ? OR id IN ?)", wholeKBs, documentIDs)
	case len(wholeKBs) > 0:
		query = query.Where("knowledge_base_id IN ?", wholeKBs)
	default:
		query = query.Where("id IN ?", documentIDs)
	}
	if keyword := strings.ToLower(strings.TrimSpace(input.Query)); keyword != "" {
		like := "%" + keyword + "%"
		query = query.Where("LOWER(file_name) LIKE ? OR LOWER(title) LIKE ?", like, like)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, err
	}
	var rows []types.Knowledge
	if err := query.Order("updated_at DESC, id ASC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(rows))
	kbIDs := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
		kbIDs = append(kbIDs, rows[i].KnowledgeBaseID)
	}
	tagMap, _ := s.knowledgeService.GetKnowledgeTags(ctx, ids)
	kbNames := make(map[string]string)
	if kbs, err := s.kbService.GetKnowledgeBasesByIDsOnly(ctx, compactUnique(kbIDs)); err == nil {
		for _, kb := range kbs {
			if kb != nil {
				kbNames[kb.ID] = kb.Name
			}
		}
	}

	views := make([]DocumentView, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		view := DocumentView{
			ID:                row.ID,
			KnowledgeBaseID:   row.KnowledgeBaseID,
			KnowledgeBaseName: kbNames[row.KnowledgeBaseID],
			Title:             row.Title,
			FileName:          row.FileName,
			FileType:          row.FileType,
			FileSize:          row.FileSize,
			FileHash:          row.FileHash,
			ParseStatus:       row.ParseStatus,
			PendingSubtasks:   row.PendingSubtasksCount,
			Metadata:          row.GetMetadata(),
			CreatedAt:         row.CreatedAt,
			UpdatedAt:         row.UpdatedAt,
		}
		for _, tag := range tagMap[row.ID] {
			if tag != nil {
				view.Tags = append(view.Tags, TagView{ID: tag.ID, Name: tag.Name})
			}
		}
		views = append(views, view)
	}
	return &DocumentInventory{Total: total, Page: page, PageSize: pageSize, Documents: views}, nil
}
