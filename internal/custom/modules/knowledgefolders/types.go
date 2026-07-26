package knowledgefolders

import (
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	MaxFolderDepth     = 32
	MaxFolderNameRunes = 120
	DefaultPageSize    = 20
	MaxPageSize        = 100
)

// Folder is a management-only hierarchy node. It deliberately owns no
// retrieval fields: documents remain the only retrievable knowledge objects.
type Folder struct {
	ID              string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID        uint64    `json:"tenant_id" gorm:"not null;index:idx_custom_knowledge_folder_scope,priority:1;uniqueIndex:idx_custom_knowledge_folder_sibling,priority:1"`
	KnowledgeBaseID string    `json:"knowledge_base_id" gorm:"type:varchar(36);not null;index:idx_custom_knowledge_folder_scope,priority:2;uniqueIndex:idx_custom_knowledge_folder_sibling,priority:2"`
	ParentID        string    `json:"parent_id" gorm:"type:varchar(36);not null;default:'';index:idx_custom_knowledge_folder_scope,priority:3;uniqueIndex:idx_custom_knowledge_folder_sibling,priority:3"`
	Name            string    `json:"name" gorm:"type:varchar(255);not null"`
	NormalizedName  string    `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_custom_knowledge_folder_sibling,priority:4"`
	Description     string    `json:"description" gorm:"type:text;not null;default:''"`
	Path            string    `json:"path" gorm:"type:varchar(4096);not null;default:'';index"`
	Depth           int       `json:"depth" gorm:"not null;default:1;index"`
	SortOrder       int       `json:"sort_order" gorm:"not null;default:0;index"`
	CreatedBy       string    `json:"created_by,omitempty" gorm:"type:varchar(36);not null;default:''"`
	UpdatedBy       string    `json:"updated_by,omitempty" gorm:"type:varchar(36);not null;default:''"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (Folder) TableName() string { return "custom_knowledge_folders" }

// FolderClosure stores the transitive closure including one depth=0 self row.
// It makes cycle checks, ancestor updates and subtree moves bounded by hierarchy
// size instead of document count.
type FolderClosure struct {
	TenantID        uint64 `json:"tenant_id" gorm:"not null;index:idx_custom_knowledge_folder_closure_scope,priority:1"`
	KnowledgeBaseID string `json:"knowledge_base_id" gorm:"type:varchar(36);not null;index:idx_custom_knowledge_folder_closure_scope,priority:2"`
	AncestorID      string `json:"ancestor_id" gorm:"type:varchar(36);primaryKey;index"`
	DescendantID    string `json:"descendant_id" gorm:"type:varchar(36);primaryKey;index"`
	Depth           int    `json:"depth" gorm:"not null;index"`
}

func (FolderClosure) TableName() string { return "custom_knowledge_folder_closure" }

// FolderStats is the read model served to the UI. These values are maintained
// incrementally and repaired by reconciliation; list/search requests never
// recursively count documents or processing tasks.
type FolderStats struct {
	FolderID                   string    `json:"folder_id" gorm:"type:varchar(36);primaryKey"`
	TenantID                   uint64    `json:"tenant_id" gorm:"not null;index:idx_custom_knowledge_folder_stats_scope,priority:1"`
	KnowledgeBaseID            string    `json:"knowledge_base_id" gorm:"type:varchar(36);not null;index:idx_custom_knowledge_folder_stats_scope,priority:2"`
	DirectChildFolderCount     int64     `json:"direct_child_folder_count" gorm:"not null;default:0"`
	SubtreeDocumentCount       int64     `json:"subtree_document_count" gorm:"not null;default:0"`
	ParsePendingCount          int64     `json:"parse_pending_count" gorm:"not null;default:0"`
	ParseRunningCount          int64     `json:"parse_running_count" gorm:"not null;default:0"`
	EnrichmentPendingTaskCount int64     `json:"enrichment_pending_task_count" gorm:"not null;default:0"`
	WikiPendingTaskCount       int64     `json:"wiki_pending_task_count" gorm:"not null;default:0"`
	AbnormalDocumentCount      int64     `json:"abnormal_document_count" gorm:"not null;default:0"`
	UpdatedAt                  time.Time `json:"stats_updated_at"`
}

func (FolderStats) TableName() string { return "custom_knowledge_folder_stats" }

type FolderView struct {
	Folder
	Stats FolderStats `json:"stats" gorm:"-"`
}

type Breadcrumb struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Node struct {
	NodeType          string           `json:"node_type"`
	KnowledgeBaseID   string           `json:"knowledge_base_id,omitempty"`
	KnowledgeBaseName string           `json:"knowledge_base_name,omitempty"`
	Folder            *FolderView      `json:"folder,omitempty"`
	Document          *types.Knowledge `json:"document,omitempty"`
}

type NodePage struct {
	Data        []Node       `json:"data"`
	Total       int64        `json:"total"`
	Page        int          `json:"page"`
	PageSize    int          `json:"page_size"`
	Current     *FolderView  `json:"current_folder,omitempty"`
	Breadcrumbs []Breadcrumb `json:"breadcrumbs"`
}

type CreateFolderRequest struct {
	ParentID    string `json:"parent_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	SortOrder   int    `json:"sort_order"`
}

type UpdateFolderRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	ParentID    *string `json:"parent_id"`
	SortOrder   *int    `json:"sort_order"`
}

type MoveDocumentsRequest struct {
	KnowledgeIDs   []string `json:"knowledge_ids"`
	TargetFolderID string   `json:"target_folder_id"`
}

type FolderSearchPage struct {
	Data     []Node `json:"data"`
	Total    int64  `json:"total"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
}

type URLKnowledgeRequest struct {
	URL              string                           `json:"url" binding:"required"`
	FileName         string                           `json:"file_name"`
	FileType         string                           `json:"file_type"`
	EnableMultimodel *bool                            `json:"enable_multimodel"`
	Title            string                           `json:"title"`
	TagIDs           []string                         `json:"tag_ids"`
	Channel          string                           `json:"channel"`
	ProcessConfig    *types.KnowledgeProcessOverrides `json:"process_config"`
	FolderID         string                           `json:"folder_id"`
}

type ManualKnowledgeRequest struct {
	types.ManualKnowledgePayload
	FolderID string `json:"folder_id"`
}

type FolderOption struct {
	ID       string         `json:"id"`
	ParentID string         `json:"parent_id"`
	Name     string         `json:"name"`
	Path     string         `json:"path"`
	Depth    int            `json:"depth"`
	Stats    FolderStats    `json:"stats"`
	Children []FolderOption `json:"children,omitempty"`
}
