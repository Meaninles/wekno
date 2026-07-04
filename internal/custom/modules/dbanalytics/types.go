package dbanalytics

import (
	"encoding/json"
	"log"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	SourceTypeMySQL    = "mysql"
	SourceTypePostgres = "postgres"

	SourceStatusActive = "active"
	SourceStatusError  = "error"

	ObjectTypeTable = "table"
	ObjectTypeView  = "view"

	QueryModeLive     = "live"
	QueryModeSnapshot = "snapshot"

	ToolDBCatalog = "db_catalog"
	ToolDBSchema  = "db_schema"
	ToolDBQuery   = "db_query"

	DisplayTypeStructuredAnalysis = "structured_analysis_result"
)

type SourceConfig struct {
	Host     string            `json:"host"`
	Port     int               `json:"port"`
	Database string            `json:"database"`
	Username string            `json:"username"`
	Password string            `json:"password,omitempty"`
	SSLMode  string            `json:"ssl_mode,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
}

func (c SourceConfig) redacted() SourceConfig {
	c.Password = ""
	return c
}

type Source struct {
	ID             string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID       uint64         `json:"tenant_id" gorm:"index;not null"`
	Name           string         `json:"name" gorm:"type:varchar(255);not null"`
	Description    string         `json:"description" gorm:"type:text"`
	Type           string         `json:"type" gorm:"type:varchar(32);index;not null"`
	Config         types.JSON     `json:"-" gorm:"type:jsonb"`
	Status         string         `json:"status" gorm:"type:varchar(32);not null;default:'active';index"`
	ErrorMessage   string         `json:"error_message" gorm:"type:text"`
	QueryMode      string         `json:"query_mode" gorm:"type:varchar(32);not null;default:'live'"`
	MaxRows        int            `json:"max_rows" gorm:"not null;default:1000"`
	MaxScanRows    int            `json:"max_scan_rows" gorm:"not null;default:50000"`
	TimeoutSeconds int            `json:"timeout_seconds" gorm:"not null;default:30"`
	CreatedBy      string         `json:"created_by" gorm:"type:varchar(36)"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	Tables []SourceTable `json:"tables,omitempty" gorm:"foreignKey:SourceID"`
}

func (Source) TableName() string { return "custom_db_sources" }

func (s *Source) BeforeCreate(tx *gorm.DB) error {
	if s.ID == "" {
		s.ID = uuid.New().String()
	}
	if s.Status == "" {
		s.Status = SourceStatusActive
	}
	if s.QueryMode == "" {
		s.QueryMode = QueryModeLive
	}
	if s.MaxRows <= 0 {
		s.MaxRows = 1000
	}
	if s.MaxScanRows <= 0 {
		s.MaxScanRows = 50000
	}
	if s.TimeoutSeconds <= 0 {
		s.TimeoutSeconds = 30
	}
	return nil
}

func (s *Source) SetConfig(cfg SourceConfig) error {
	out := cfg
	if key := utils.GetAESKey(); key != nil && out.Password != "" {
		if enc, err := utils.EncryptAESGCM(out.Password, key); err == nil {
			out.Password = enc
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	s.Config = types.JSON(b)
	return nil
}

func (s *Source) ParseConfig() (SourceConfig, error) {
	var cfg SourceConfig
	if len(s.Config) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(s.Config, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Password != "" {
		if plain, ok := utils.DecryptStoredSecretLenient(cfg.Password); ok {
			cfg.Password = plain
		} else {
			log.Printf("[crypto] custom db source %s password decrypt failed, treating as empty", s.ID)
			cfg.Password = ""
		}
	}
	return cfg, nil
}

func (s *Source) Response(includeConfig bool) SourceResponse {
	resp := SourceResponse{
		ID:             s.ID,
		TenantID:       s.TenantID,
		Name:           s.Name,
		Description:    s.Description,
		Type:           s.Type,
		Status:         s.Status,
		ErrorMessage:   s.ErrorMessage,
		QueryMode:      s.QueryMode,
		MaxRows:        s.MaxRows,
		MaxScanRows:    s.MaxScanRows,
		TimeoutSeconds: s.TimeoutSeconds,
		CreatedBy:      s.CreatedBy,
		CreatedAt:      s.CreatedAt,
		UpdatedAt:      s.UpdatedAt,
	}
	if includeConfig {
		if cfg, err := s.ParseConfig(); err == nil {
			resp.Config = cfg.redacted()
		}
	}
	return resp
}

type SourceFromAgentInfo struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
}

type SourceResponse struct {
	ID             string       `json:"id"`
	TenantID       uint64       `json:"tenant_id"`
	Name           string       `json:"name"`
	Description    string       `json:"description"`
	Type           string       `json:"type"`
	Status         string       `json:"status"`
	ErrorMessage   string       `json:"error_message,omitempty"`
	QueryMode      string       `json:"query_mode"`
	MaxRows        int          `json:"max_rows"`
	MaxScanRows    int          `json:"max_scan_rows"`
	TimeoutSeconds int          `json:"timeout_seconds"`
	CreatedBy      string       `json:"created_by,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
	Config         SourceConfig `json:"config,omitempty"`

	Shared          bool                 `json:"shared,omitempty"`
	ShareID         string               `json:"share_id,omitempty"`
	OrganizationID  string               `json:"organization_id,omitempty"`
	OrgName         string               `json:"org_name,omitempty"`
	Permission      types.OrgMemberRole  `json:"permission,omitempty"`
	SourceTenantID  uint64               `json:"source_tenant_id,omitempty"`
	IsMine          bool                 `json:"is_mine,omitempty"`
	SourceFromAgent *SourceFromAgentInfo `json:"source_from_agent,omitempty"`
}

type SourceShare struct {
	ID             string              `json:"id" gorm:"type:varchar(36);primaryKey"`
	SourceID       string              `json:"source_id" gorm:"type:varchar(36);not null;index"`
	OrganizationID string              `json:"organization_id" gorm:"type:varchar(36);not null;index"`
	SharedByUserID string              `json:"shared_by_user_id" gorm:"type:varchar(36);not null"`
	SourceTenantID uint64              `json:"source_tenant_id" gorm:"not null;index"`
	Permission     types.OrgMemberRole `json:"permission" gorm:"type:varchar(32);not null;default:'viewer'"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	DeletedAt      gorm.DeletedAt      `json:"-" gorm:"index"`

	Source       *Source             `json:"source,omitempty" gorm:"foreignKey:SourceID;references:ID"`
	Organization *types.Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	SharedByUser *types.User         `json:"shared_by_user,omitempty" gorm:"foreignKey:SharedByUserID"`
}

func (SourceShare) TableName() string { return "custom_db_source_shares" }

func (s *SourceShare) BeforeCreate(tx *gorm.DB) error {
	if s.ID == "" {
		s.ID = uuid.New().String()
	}
	return nil
}

type SharedSourceInfo struct {
	Source         *SourceResponse     `json:"source"`
	ShareID        string              `json:"share_id"`
	OrganizationID string              `json:"organization_id"`
	OrgName        string              `json:"org_name"`
	Permission     types.OrgMemberRole `json:"permission"`
	SourceTenantID uint64              `json:"source_tenant_id"`
	SharedAt       time.Time           `json:"shared_at"`
}

type OrganizationSharedSourceItem struct {
	SharedSourceInfo
	IsMine          bool                 `json:"is_mine"`
	SourceFromAgent *SourceFromAgentInfo `json:"source_from_agent,omitempty"`
}

type SourceShareResponse struct {
	ID               string    `json:"id"`
	SourceID         string    `json:"source_id"`
	SourceName       string    `json:"source_name"`
	SourceType       string    `json:"source_type"`
	OrganizationID   string    `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
	SharedByUserID   string    `json:"shared_by_user_id"`
	SharedByUsername string    `json:"shared_by_username"`
	SourceTenantID   uint64    `json:"source_tenant_id"`
	Permission       string    `json:"permission"`
	MyRoleInOrg      string    `json:"my_role_in_org"`
	MyPermission     string    `json:"my_permission"`
	CreatedAt        time.Time `json:"created_at"`
	RequireApproval  bool      `json:"require_approval"`
}

type SourceTable struct {
	ID             string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	SourceID       string         `json:"source_id" gorm:"type:varchar(36);index;not null;uniqueIndex:idx_custom_db_table_ref"`
	TenantID       uint64         `json:"tenant_id" gorm:"index;not null"`
	SchemaName     string         `json:"schema_name" gorm:"type:varchar(255);not null;uniqueIndex:idx_custom_db_table_ref"`
	PhysicalName   string         `json:"table_name" gorm:"column:table_name;type:varchar(255);not null;uniqueIndex:idx_custom_db_table_ref"`
	ObjectType     string         `json:"object_type" gorm:"type:varchar(32);not null"`
	VirtualName    string         `json:"virtual_name" gorm:"type:varchar(255);index;not null"`
	Enabled        bool           `json:"enabled" gorm:"not null;default:false;index"`
	RowEstimate    int64          `json:"row_estimate"`
	Description    string         `json:"description" gorm:"type:text"`
	LastProfiledAt *time.Time     `json:"last_profiled_at"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	Columns []SourceColumn `json:"columns,omitempty" gorm:"foreignKey:TableID"`
}

func (SourceTable) TableName() string { return "custom_db_source_tables" }

func (t *SourceTable) BeforeCreate(tx *gorm.DB) error {
	if t.ID == "" {
		t.ID = uuid.New().String()
	}
	return nil
}

type SourceColumn struct {
	ID             string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TableID        string         `json:"table_id" gorm:"type:varchar(36);index;not null;uniqueIndex:idx_custom_db_col_ref"`
	SourceID       string         `json:"source_id" gorm:"type:varchar(36);index;not null"`
	TenantID       uint64         `json:"tenant_id" gorm:"index;not null"`
	ColumnName     string         `json:"column_name" gorm:"type:varchar(255);not null;uniqueIndex:idx_custom_db_col_ref"`
	DataType       string         `json:"data_type" gorm:"type:varchar(255);not null"`
	Nullable       bool           `json:"nullable"`
	Ordinal        int            `json:"ordinal"`
	Description    string         `json:"description" gorm:"type:text"`
	SampleValues   types.JSON     `json:"sample_values" gorm:"type:jsonb"`
	SemanticType   string         `json:"semantic_type" gorm:"type:varchar(32)"`
	SensitiveLevel string         `json:"sensitive_level" gorm:"type:varchar(32);not null;default:'none'"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`
}

func (SourceColumn) TableName() string { return "custom_db_source_columns" }

func (c *SourceColumn) BeforeCreate(tx *gorm.DB) error {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	return nil
}

type TableRelation struct {
	ID          string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID    uint64         `json:"tenant_id" gorm:"index;not null"`
	SourceID    string         `json:"source_id" gorm:"type:varchar(36);index;not null"`
	FromTableID string         `json:"from_table_id" gorm:"type:varchar(36);index;not null"`
	FromColumn  string         `json:"from_column" gorm:"type:varchar(255);not null"`
	ToTableID   string         `json:"to_table_id" gorm:"type:varchar(36);index;not null"`
	ToColumn    string         `json:"to_column" gorm:"type:varchar(255);not null"`
	Description string         `json:"description" gorm:"type:text"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`
}

func (TableRelation) TableName() string { return "custom_db_table_relations" }

func (r *TableRelation) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	return nil
}

type AgentBinding struct {
	ID        string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID  uint64         `json:"tenant_id" gorm:"index;not null;uniqueIndex:idx_custom_db_agent_source"`
	AgentID   string         `json:"agent_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_custom_db_agent_source"`
	SourceID  string         `json:"source_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_custom_db_agent_source"`
	Enabled   bool           `json:"enabled" gorm:"not null;default:true;index"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (AgentBinding) TableName() string { return "custom_db_agent_bindings" }

func (b *AgentBinding) BeforeCreate(tx *gorm.DB) error {
	if b.ID == "" {
		b.ID = uuid.New().String()
	}
	return nil
}

type QueryAudit struct {
	ID             string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID       uint64         `json:"tenant_id" gorm:"index;not null"`
	UserID         string         `json:"user_id" gorm:"type:varchar(36);index"`
	AgentID        string         `json:"agent_id" gorm:"type:varchar(36);index"`
	SourceID       string         `json:"source_id" gorm:"type:varchar(36);index"`
	OriginalSQL    string         `json:"original_sql" gorm:"type:text"`
	ExecutedSQL    string         `json:"executed_sql" gorm:"type:text"`
	QueryMode      string         `json:"query_mode" gorm:"type:varchar(32)"`
	ChartRequested bool           `json:"chart_requested"`
	DurationMS     int64          `json:"duration_ms"`
	RowCount       int            `json:"row_count"`
	Success        bool           `json:"success"`
	ErrorMessage   string         `json:"error_message" gorm:"type:text"`
	CreatedAt      time.Time      `json:"created_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`
}

func (QueryAudit) TableName() string { return "custom_db_query_audits" }

func (a *QueryAudit) BeforeCreate(tx *gorm.DB) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	return nil
}

type CreateSourceRequest struct {
	Name           string       `json:"name" binding:"required"`
	Description    string       `json:"description"`
	Type           string       `json:"type" binding:"required"`
	Config         SourceConfig `json:"config" binding:"required"`
	QueryMode      string       `json:"query_mode"`
	MaxRows        int          `json:"max_rows"`
	MaxScanRows    int          `json:"max_scan_rows"`
	TimeoutSeconds int          `json:"timeout_seconds"`
}

type TestSourceRequest struct {
	Type           string       `json:"type" binding:"required"`
	Config         SourceConfig `json:"config" binding:"required"`
	TimeoutSeconds int          `json:"timeout_seconds"`
}

type UpdateSourceRequest struct {
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	Config         *SourceConfig `json:"config"`
	QueryMode      string        `json:"query_mode"`
	MaxRows        int           `json:"max_rows"`
	MaxScanRows    int           `json:"max_scan_rows"`
	TimeoutSeconds int           `json:"timeout_seconds"`
}

type SetTableScopeRequest struct {
	TableIDs []string `json:"table_ids"`
}

type UpdateColumnRequest struct {
	Description    string `json:"description"`
	SemanticType   string `json:"semantic_type"`
	SensitiveLevel string `json:"sensitive_level"`
}

type AgentBindingRequest struct {
	SourceIDs []string `json:"source_ids"`
}

type ShareSourceRequest struct {
	OrganizationID string              `json:"organization_id" binding:"required"`
	Permission     types.OrgMemberRole `json:"permission" binding:"required"`
}

type UpdateSourceSharePermissionRequest struct {
	Permission types.OrgMemberRole `json:"permission" binding:"required"`
}

type TableRef struct {
	SchemaName  string
	TableName   string
	VirtualName string
}

type ColumnInfo struct {
	Name           string   `json:"name"`
	DataType       string   `json:"data_type"`
	Nullable       bool     `json:"nullable"`
	Ordinal        int      `json:"ordinal"`
	Description    string   `json:"description,omitempty"`
	SampleValues   []string `json:"sample_values,omitempty"`
	SemanticType   string   `json:"semantic_type,omitempty"`
	SensitiveLevel string   `json:"sensitive_level,omitempty"`
}

type TableProfile struct {
	SchemaName  string       `json:"schema_name"`
	TableName   string       `json:"table_name"`
	ObjectType  string       `json:"object_type"`
	RowEstimate int64        `json:"row_estimate"`
	Description string       `json:"description,omitempty"`
	Columns     []ColumnInfo `json:"columns"`
}

type QueryInput struct {
	SourceID        string `json:"source_id,omitempty" jsonschema:"optional database source id copied from db_catalog/db_schema; omit it when using sql_table_name values across bound sources"`
	SQL             string `json:"sql" jsonschema:"DuckDB-compatible read-only SQL to run against materialized database analysis tables; use sql_table_name values from db_catalog/db_schema"`
	ChartRequested  bool   `json:"chart_requested,omitempty" jsonschema:"true only when the user explicitly asks for a chart, graph, plot, visualization, 图表, 可视化, or a named chart type"`
	PreferredChart  string `json:"preferred_chart,omitempty" jsonschema:"optional chart type requested by the user or selected after an explicit chart request: line,bar,stacked_bar,pie,scatter,histogram,heatmap,funnel,dual_axis_combo,area,radar,treemap,boxplot"`
	ChartIntent     string `json:"chart_intent,omitempty" jsonschema:"optional natural-language chart intent, e.g. compare regional sales by customer segment; used only when chart_requested is true"`
	PrimaryMetric   string `json:"primary_metric,omitempty" jsonschema:"optional SQL result column name that should be the primary visual metric when chart_requested is true"`
	SecondaryMetric string `json:"secondary_metric,omitempty" jsonschema:"optional SQL result column name for a secondary metric, especially dual_axis_combo or relationship charts"`
	Dimension       string `json:"dimension,omitempty" jsonschema:"optional SQL result column name that should be the main category/time axis when chart_requested is true"`
	Series          string `json:"series,omitempty" jsonschema:"optional SQL result column name that should be the series/stack/group dimension when chart_requested is true"`
	ChartTitle      string `json:"chart_title,omitempty" jsonschema:"optional concise Chinese chart title; use when it helps align the rendered chart with the final explanation"`
	TableRequested  bool   `json:"table_requested,omitempty" jsonschema:"true only when the user explicitly asks for table/detail/raw/list output; false by default even when a chart is requested"`
}

type SchemaInput struct {
	SourceID   string   `json:"source_id,omitempty" jsonschema:"optional database source id copied from db_catalog/db_schema; omit it when table_names contains sql_table_name values"`
	TableNames []string `json:"table_names,omitempty" jsonschema:"optional sql_table_name values returned by db_catalog; physical schema.table names are also accepted"`
}

type CatalogInput struct {
	Query    string `json:"query" jsonschema:"user question or business terms to match against tables, columns and descriptions"`
	SourceID string `json:"source_id,omitempty" jsonschema:"optional database source id"`
}
