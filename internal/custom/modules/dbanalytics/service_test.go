package dbanalytics

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	wktypes "github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestVirtualTableNameAppendsHashBeforeLengthLimit(t *testing.T) {
	src := &Source{
		ID:   "12345678-aaaa-bbbb-cccc-1234567890ab",
		Type: SourceTypeMySQL,
	}
	schema := strings.Repeat("very_long_schema_", 10)
	left := virtualTableName(src, schema, strings.Repeat("same_prefix_", 20)+"left")
	right := virtualTableName(src, schema, strings.Repeat("same_prefix_", 20)+"right")

	if left == right {
		t.Fatalf("virtual table names collided: %q", left)
	}
	for _, got := range []string{left, right} {
		if len(got) > maxVirtualIdentLength {
			t.Fatalf("virtual table name length = %d, want <= %d: %q", len(got), maxVirtualIdentLength, got)
		}
		if !regexp.MustCompile(`__[0-9a-f]{8}$`).MatchString(got) {
			t.Fatalf("virtual table name missing hash suffix: %q", got)
		}
	}
}

func TestTablesReferencedBySQLFiltersToReferencedVirtualNames(t *testing.T) {
	tables := []SourceTable{
		{VirtualName: "orders_v"},
		{VirtualName: "products_v"},
		{VirtualName: "customers_v"},
	}
	got := tablesReferencedBySQL(tables, []string{"CUSTOMERS_V", "orders_v"})
	if len(got) != 2 {
		t.Fatalf("referenced tables len = %d, want 2: %#v", len(got), got)
	}
	if got[0].VirtualName != "orders_v" || got[1].VirtualName != "customers_v" {
		t.Fatalf("referenced tables = %#v, want orders_v and customers_v in source order", got)
	}
}

func TestTablesReferencedBySQLScansNestedSQLText(t *testing.T) {
	tables := []SourceTable{
		{VirtualName: "orders_v"},
		{VirtualName: "products_v"},
		{VirtualName: "customers_v"},
	}
	got := tablesReferencedBySQL(
		tables,
		nil,
		`SELECT * FROM (SELECT * FROM "orders_v") o JOIN customers_v c ON o.customer_id = c.id WHERE c.note = 'products_v'`,
	)
	if len(got) != 2 {
		t.Fatalf("referenced tables len = %d, want 2: %#v", len(got), got)
	}
	if got[0].VirtualName != "orders_v" || got[1].VirtualName != "customers_v" {
		t.Fatalf("referenced tables = %#v, want orders_v and customers_v in source order", got)
	}
}

func TestInferChartSpecRequiresExplicitRequest(t *testing.T) {
	columns := []map[string]any{
		{"name": "day", "semantic_type": "time"},
		{"name": "revenue", "semantic_type": "metric"},
	}
	rows := []map[string]any{{"day": "2026-06-25", "revenue": 42.5}}

	notRequested := inferChartSpec(columns, rows, "", false, chartIntentHints{})
	if notRequested["eligible"].(bool) {
		t.Fatalf("chart should not be eligible when chart_requested is false: %#v", notRequested)
	}
	if notRequested["reason"] != "chart not requested" {
		t.Fatalf("chart reason = %q, want chart not requested", notRequested["reason"])
	}

	requested := inferChartSpec(columns, rows, "", true, chartIntentHints{})
	if !requested["eligible"].(bool) {
		t.Fatalf("chart should be eligible when explicitly requested: %#v", requested)
	}
}

func TestInferChartSpecSupportsStructuredChartTypes(t *testing.T) {
	tests := []struct {
		name      string
		preferred string
		columns   []map[string]any
		rows      []map[string]any
		wantType  string
	}{
		{
			name:      "dual axis default for time plus two metrics",
			preferred: "",
			columns: []map[string]any{
				{"name": "day", "semantic_type": "time"},
				{"name": "revenue", "semantic_type": "metric"},
				{"name": "order_count", "semantic_type": "metric"},
			},
			rows:     []map[string]any{{"day": "2026-06-25", "revenue": 42.5, "order_count": 7}},
			wantType: "dual_axis_combo",
		},
		{
			name:      "area",
			preferred: "area",
			columns: []map[string]any{
				{"name": "day", "semantic_type": "time"},
				{"name": "revenue", "semantic_type": "metric"},
			},
			rows:     []map[string]any{{"day": "2026-06-25", "revenue": 42.5}},
			wantType: "area",
		},
		{
			name:      "radar",
			preferred: "radar",
			columns: []map[string]any{
				{"name": "segment", "semantic_type": "dimension"},
				{"name": "revenue", "semantic_type": "metric"},
				{"name": "order_count", "semantic_type": "metric"},
				{"name": "profit", "semantic_type": "metric"},
			},
			rows:     []map[string]any{{"segment": "vip", "revenue": 42.5, "order_count": 7, "profit": 9.1}},
			wantType: "radar",
		},
		{
			name:      "treemap",
			preferred: "treemap",
			columns: []map[string]any{
				{"name": "region", "semantic_type": "dimension"},
				{"name": "channel", "semantic_type": "dimension"},
				{"name": "revenue", "semantic_type": "metric"},
			},
			rows:     []map[string]any{{"region": "华东", "channel": "app", "revenue": 42.5}},
			wantType: "treemap",
		},
		{
			name:      "boxplot",
			preferred: "boxplot",
			columns: []map[string]any{
				{"name": "segment", "semantic_type": "dimension"},
				{"name": "pay_amount", "semantic_type": "metric"},
			},
			rows:     []map[string]any{{"segment": "vip", "pay_amount": 42.5}},
			wantType: "boxplot",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferChartSpec(tt.columns, tt.rows, tt.preferred, true, chartIntentHints{})
			if !got["eligible"].(bool) {
				t.Fatalf("chart should be eligible: %#v", got)
			}
			if got["default_type"] != tt.wantType {
				t.Fatalf("chart type = %v, want %s: %#v", got["default_type"], tt.wantType, got)
			}
			if got["id"] == "" {
				t.Fatalf("chart id should be populated: %#v", got)
			}
		})
	}
}

func TestInferChartSpecBuildsGenericChartContract(t *testing.T) {
	columns := []map[string]any{
		{"name": "region", "semantic_type": "dimension"},
		{"name": "segment", "semantic_type": "dimension"},
		{"name": "amount", "semantic_type": "metric"},
	}
	rows := []map[string]any{
		{"region": "东区", "segment": "VIP", "amount": 100},
		{"region": "东区", "segment": "VIP", "amount": 50},
	}

	got := inferChartSpec(columns, rows, "heatmap", true, chartIntentHints{})
	if !got["eligible"].(bool) {
		t.Fatalf("chart should be eligible: %#v", got)
	}
	contract, ok := got["contract"].(map[string]any)
	if !ok {
		t.Fatalf("chart contract missing: %#v", got)
	}
	if contract["type"] != "heatmap" {
		t.Fatalf("contract type = %v, want heatmap", contract["type"])
	}
	encoding := contract["encoding"].(map[string]any)
	for key, want := range map[string]string{"x": "region", "y": "segment", "value": "amount"} {
		if gotField := encodingFieldName(encoding, key); gotField != want {
			t.Fatalf("encoding.%s = %q, want %q in contract %#v", key, gotField, want, contract)
		}
	}
	transform := contract["transform"].(map[string]any)
	groupBy := transform["group_by"].([]string)
	if strings.Join(groupBy, ",") != "region,segment" {
		t.Fatalf("contract group_by = %#v, want region + segment", groupBy)
	}
	display := contract["display"].(map[string]any)
	if display["language"] != "zh-CN" || display["table_visible"] != false {
		t.Fatalf("contract display = %#v, want zh-CN and hidden table", display)
	}
	validation := got["validation"].(map[string]any)
	if validation["status"] != "pass" {
		t.Fatalf("contract validation = %#v, want pass", validation)
	}
}

func TestInferChartSpecUsesIntentHintsAndEvidenceScope(t *testing.T) {
	columns := []map[string]any{
		{"name": "区域", "semantic_type": "dimension"},
		{"name": "客户类型", "semantic_type": "dimension"},
		{"name": "销售额", "semantic_type": "metric"},
		{"name": "订单数", "semantic_type": "metric"},
	}
	rows := []map[string]any{
		{"区域": "东区", "客户类型": "VIP", "销售额": 1000, "订单数": 8},
	}

	got := inferChartSpec(columns, rows, "stacked_bar", true, chartIntentHints{
		intent:        "比较各区域不同客户类型的订单数构成",
		dimension:     "区域",
		series:        "客户类型",
		primaryMetric: "订单数",
		title:         "各区域客户类型订单数构成",
	})
	if !got["eligible"].(bool) {
		t.Fatalf("chart should be eligible: %#v", got)
	}
	contract := got["contract"].(map[string]any)
	encoding := contract["encoding"].(map[string]any)
	if gotField := encodingFieldName(encoding, "value"); gotField != "订单数" {
		t.Fatalf("encoding.value = %q, want 订单数 in contract %#v", gotField, contract)
	}
	if gotField := encodingFieldName(encoding, "series"); gotField != "客户类型" {
		t.Fatalf("encoding.series = %q, want 客户类型 in contract %#v", gotField, contract)
	}
	intent := contract["intent"].(map[string]any)
	if intent["reason"] != "比较各区域不同客户类型的订单数构成" {
		t.Fatalf("intent reason = %#v", intent)
	}
	display := contract["display"].(map[string]any)
	if display["title"] != "各区域客户类型订单数构成" {
		t.Fatalf("display title = %#v", display["title"])
	}
	visualScope := contract["visual_scope"].(map[string]any)
	visualMetrics := visualScope["metrics"].([]string)
	if strings.Join(visualMetrics, ",") != "订单数" {
		t.Fatalf("visual metrics = %#v, want 订单数", visualMetrics)
	}
	evidenceScope := contract["evidence_scope"].(map[string]any)
	nonVisualFields := evidenceScope["non_visual_fields"].([]string)
	if !containsString(nonVisualFields, "销售额") {
		t.Fatalf("non visual fields = %#v, want 销售额 retained as textual evidence", nonVisualFields)
	}
}

func TestInferChartSpecContractPreservesMultiMetricFields(t *testing.T) {
	columns := []map[string]any{
		{"name": "segment", "semantic_type": "dimension"},
		{"name": "amount", "semantic_type": "metric"},
		{"name": "orders", "semantic_type": "metric"},
		{"name": "profit", "semantic_type": "metric"},
	}
	rows := []map[string]any{{"segment": "VIP", "amount": 100, "orders": 8, "profit": 30}}

	got := inferChartSpec(columns, rows, "radar", true, chartIntentHints{})
	if !got["eligible"].(bool) {
		t.Fatalf("chart should be eligible: %#v", got)
	}
	contract := got["contract"].(map[string]any)
	encoding := contract["encoding"].(map[string]any)
	values := encodingFieldNames(encoding, "values")
	if strings.Join(values, ",") != "amount,orders,profit" {
		t.Fatalf("contract values = %#v, want all metrics", values)
	}
	validation := got["validation"].(map[string]any)
	if validation["status"] != "pass" {
		t.Fatalf("contract validation = %#v, want pass", validation)
	}
}

func TestFormatAnalysisOutputProvidesChartAnchorWithoutTableSuppressionRule(t *testing.T) {
	output := formatAnalysisOutput(map[string]any{
		"query":           `SELECT day, revenue FROM orders_v`,
		"display_mode":    "chart_only",
		"chart_requested": true,
		"chart": map[string]any{
			"eligible":     true,
			"id":           "chart_abc123",
			"default_type": "line",
			"x":            "day",
			"y":            []string{"revenue"},
		},
		"rows": []map[string]any{{"day": "2026-06-25", "revenue": 42.5}},
	})

	if !strings.Contains(output, "{{chart:chart_abc123}}") {
		t.Fatalf("analysis output missing explicit chart anchor: %s", output)
	}
	if strings.Contains(output, "do not render these rows as a Markdown table") {
		t.Fatalf("analysis output should not include no-table rule: %s", output)
	}
}

func TestDBSchemaSemanticContextStrippedFromClientPayload(t *testing.T) {
	result := &wktypes.ToolResult{
		Success: true,
		Output:  "internal semantic reasoning",
		Data: map[string]interface{}{
			"display_type":     "db_schema",
			"count":            1,
			"semantic_context": []map[string]any{{"business_meaning": "internal"}},
		},
	}

	clientMeta := agenttools.SanitizeToolResultForClient(ToolDBSchema, result)
	if _, ok := clientMeta["semantic_context"]; ok {
		t.Fatalf("semantic_context leaked to client metadata: %#v", clientMeta)
	}
	if _, ok := clientMeta["output"]; ok {
		t.Fatalf("raw schema tool output leaked to client metadata: %#v", clientMeta)
	}
	persisted := agenttools.SanitizeToolDataForPersist(result.Data)
	if _, ok := persisted["semantic_context"]; ok {
		t.Fatalf("semantic_context leaked to persisted tool data: %#v", persisted)
	}
}

func newDBAnalyticsSharingTestService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db, nil)
	ctx := context.Background()
	if err := db.WithContext(ctx).AutoMigrate(
		&wktypes.Organization{},
		&wktypes.OrganizationTenantMember{},
		&wktypes.CustomAgent{},
		&wktypes.AgentShare{},
	); err != nil {
		t.Fatalf("migrate shared models: %v", err)
	}
	if err := svc.Migrate(ctx); err != nil {
		t.Fatalf("migrate dbanalytics: %v", err)
	}
	return svc, ctx
}

func seedSharingOrganization(t *testing.T, svc *Service, ctx context.Context) {
	t.Helper()
	now := time.Now()
	rows := []any{
		&wktypes.Organization{ID: "org-1", Name: "Analysis Space", OwnerID: "user-1", OwnerTenantID: 1},
		&wktypes.OrganizationTenantMember{ID: "member-1", OrganizationID: "org-1", TenantID: 1, Role: wktypes.OrgRoleEditor, JoinedAt: &now},
		&wktypes.OrganizationTenantMember{ID: "member-2", OrganizationID: "org-1", TenantID: 2, Role: wktypes.OrgRoleViewer, JoinedAt: &now},
	}
	for _, row := range rows {
		if err := svc.db.WithContext(ctx).Create(row).Error; err != nil {
			t.Fatalf("seed organization row: %v", err)
		}
	}
}

func TestListSharedSourcesAppliesOrganizationAndTenantRoleCaps(t *testing.T) {
	svc, ctx := newDBAnalyticsSharingTestService(t)
	seedSharingOrganization(t, svc, ctx)
	src := &Source{
		ID:             "source-1",
		TenantID:       1,
		Name:           "Orders DB",
		Type:           SourceTypePostgres,
		Status:         SourceStatusActive,
		QueryMode:      QueryModeLive,
		MaxRows:        1000,
		MaxScanRows:    50000,
		TimeoutSeconds: 30,
	}
	if err := svc.db.WithContext(ctx).Create(src).Error; err != nil {
		t.Fatalf("create source: %v", err)
	}

	if _, err := svc.ShareSource(ctx, src.ID, "org-1", "user-1", 1, wktypes.OrgRoleEditor); err != nil {
		t.Fatalf("share source: %v", err)
	}
	list, err := svc.ListSharedSources(ctx, 2, wktypes.TenantRoleContributor)
	if err != nil {
		t.Fatalf("list shared sources: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("shared source count = %d, want 1: %#v", len(list), list)
	}
	if list[0].Permission != wktypes.OrgRoleViewer {
		t.Fatalf("effective permission = %s, want viewer", list[0].Permission)
	}
	if list[0].Source == nil || !list[0].Source.Shared || list[0].Source.ID != src.ID {
		t.Fatalf("shared source response not marked correctly: %#v", list[0].Source)
	}
}

func TestShareSourceRejectsAdminPermission(t *testing.T) {
	svc, ctx := newDBAnalyticsSharingTestService(t)
	seedSharingOrganization(t, svc, ctx)
	src := &Source{
		ID:             "source-1",
		TenantID:       1,
		Name:           "Orders DB",
		Type:           SourceTypePostgres,
		Status:         SourceStatusActive,
		QueryMode:      QueryModeLive,
		MaxRows:        1000,
		MaxScanRows:    50000,
		TimeoutSeconds: 30,
	}
	if err := svc.db.WithContext(ctx).Create(src).Error; err != nil {
		t.Fatalf("create source: %v", err)
	}

	if _, err := svc.ShareSource(ctx, src.ID, "org-1", "user-1", 1, wktypes.OrgRoleAdmin); err != ErrInvalidSharePermission {
		t.Fatalf("share admin permission error = %v, want ErrInvalidSharePermission", err)
	}
}

func TestOrganizationSourcesIncludeDatabaseSourcesFromSharedAgent(t *testing.T) {
	svc, ctx := newDBAnalyticsSharingTestService(t)
	seedSharingOrganization(t, svc, ctx)
	src := &Source{
		ID:             "source-1",
		TenantID:       1,
		Name:           "Messy Commerce DB",
		Type:           SourceTypeMySQL,
		Status:         SourceStatusActive,
		QueryMode:      QueryModeLive,
		MaxRows:        1000,
		MaxScanRows:    50000,
		TimeoutSeconds: 30,
	}
	agent := &wktypes.CustomAgent{
		ID:       "agent-1",
		TenantID: 1,
		Name:     "Professional Data Analyst",
		Config: wktypes.CustomAgentConfig{
			AgentMode:     wktypes.AgentModeSmartReasoning,
			AgentType:     wktypes.AgentTypeDataAnalysis,
			DBDataSources: []string{src.ID},
		},
	}
	share := &wktypes.AgentShare{
		ID:             "agent-share-1",
		AgentID:        agent.ID,
		OrganizationID: "org-1",
		SharedByUserID: "user-1",
		SourceTenantID: 1,
		Permission:     wktypes.OrgRoleViewer,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	for _, row := range []any{src, agent, share} {
		if err := svc.db.WithContext(ctx).Create(row).Error; err != nil {
			t.Fatalf("seed agent source row: %v", err)
		}
	}

	list, err := svc.ListOrganizationSourcesIncludingAgent(ctx, "org-1", 2, wktypes.TenantRoleContributor)
	if err != nil {
		t.Fatalf("list organization sources: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("organization source count = %d, want 1: %#v", len(list), list)
	}
	if list[0].SourceFromAgent == nil || list[0].SourceFromAgent.AgentID != agent.ID {
		t.Fatalf("source_from_agent missing: %#v", list[0])
	}
	if list[0].ShareID != "" {
		t.Fatalf("agent-carried source should not have direct share id, got %q", list[0].ShareID)
	}
	if list[0].Source == nil || list[0].Source.ID != src.ID || !list[0].Source.Shared {
		t.Fatalf("agent-carried source response not marked shared: %#v", list[0].Source)
	}
	detail, err := svc.GetAccessibleSourceWithTables(ctx, 2, src.ID, wktypes.TenantRoleContributor)
	if err != nil {
		t.Fatalf("agent-carried source detail should be readable: %v", err)
	}
	if detail.ID != src.ID || detail.TenantID != src.TenantID {
		t.Fatalf("agent-carried source detail = %#v, want source tenant %d", detail, src.TenantID)
	}
}

func TestCatalogUsesSourceTenantForSharedAgentBoundSources(t *testing.T) {
	svc, ctx := newDBAnalyticsSharingTestService(t)
	src := &Source{
		ID:             "source-1",
		TenantID:       1,
		Name:           "Orders Warehouse",
		Type:           SourceTypePostgres,
		Status:         SourceStatusActive,
		QueryMode:      QueryModeLive,
		MaxRows:        1000,
		MaxScanRows:    50000,
		TimeoutSeconds: 30,
	}
	table := &SourceTable{
		ID:           "table-1",
		TenantID:     1,
		SourceID:     src.ID,
		SchemaName:   "public",
		PhysicalName: "messy_order_facts",
		ObjectType:   "table",
		VirtualName:  "pg_source_1__public__messy_order_facts",
		Enabled:      true,
		Description:  "Incomplete order facts table with mixed business naming",
	}
	column := &SourceColumn{
		ID:           "column-1",
		TenantID:     1,
		SourceID:     src.ID,
		TableID:      table.ID,
		ColumnName:   "amt",
		DataType:     "numeric",
		Ordinal:      1,
		Description:  "Order amount",
		SemanticType: "metric",
	}
	for _, row := range []any{src, table, column} {
		if err := svc.db.WithContext(ctx).Create(row).Error; err != nil {
			t.Fatalf("seed source metadata: %v", err)
		}
	}

	result, err := svc.Catalog(ctx, ToolScope{
		TenantID:       2,
		SourceTenantID: 1,
		TenantRole:     wktypes.TenantRoleContributor,
		AgentID:        "agent-1",
		AgentType:      wktypes.AgentTypeDataAnalysis,
		SourceIDs:      []string{src.ID},
	}, CatalogInput{Query: "order amount"})
	if err != nil {
		t.Fatalf("catalog via shared agent source tenant: %v", err)
	}
	if result["count"] != 1 {
		t.Fatalf("catalog count = %v, want 1: %#v", result["count"], result)
	}
	tables, ok := result["tables"].([]map[string]any)
	if !ok || len(tables) != 1 || tables[0]["source_id"] != src.ID {
		t.Fatalf("catalog tables not sourced from agent tenant: %#v", result["tables"])
	}
}

func TestDBQueryRequiresSchemaReasoningForRuntimeSessions(t *testing.T) {
	svc, ctx := newDBAnalyticsSharingTestService(t)

	_, err := svc.ExecuteQuery(ctx, ToolScope{
		TenantID:       1,
		SourceTenantID: 1,
		SessionID:      "session-1",
		SourceIDs:      []string{"source-1"},
	}, QueryInput{SQL: "SELECT 1"}, true)
	if err == nil || !strings.Contains(err.Error(), "db_schema") {
		t.Fatalf("ExecuteQuery error = %v, want db_schema requirement", err)
	}
}

func TestDBQueryRequiresSchemaForReferencedTables(t *testing.T) {
	svc, ctx := newDBAnalyticsSharingTestService(t)
	src := &Source{
		ID:             "source-1",
		TenantID:       1,
		Name:           "Orders Warehouse",
		Type:           SourceTypePostgres,
		Status:         SourceStatusActive,
		QueryMode:      QueryModeLive,
		MaxRows:        1000,
		MaxScanRows:    50000,
		TimeoutSeconds: 30,
	}
	orders := &SourceTable{
		ID:           "orders-table",
		TenantID:     1,
		SourceID:     src.ID,
		SchemaName:   "public",
		PhysicalName: "orders",
		ObjectType:   "table",
		VirtualName:  "orders_v",
		Enabled:      true,
	}
	customers := &SourceTable{
		ID:           "customers-table",
		TenantID:     1,
		SourceID:     src.ID,
		SchemaName:   "public",
		PhysicalName: "customers",
		ObjectType:   "table",
		VirtualName:  "customers_v",
		Enabled:      true,
	}
	columns := []*SourceColumn{
		{ID: "orders-id", TenantID: 1, SourceID: src.ID, TableID: orders.ID, ColumnName: "id", DataType: "integer", Ordinal: 1},
		{ID: "customers-id", TenantID: 1, SourceID: src.ID, TableID: customers.ID, ColumnName: "id", DataType: "integer", Ordinal: 1},
	}
	for _, row := range []any{src, orders, customers, columns[0], columns[1]} {
		if err := svc.db.WithContext(ctx).Create(row).Error; err != nil {
			t.Fatalf("seed schema reasoning rows: %v", err)
		}
	}

	scope := ToolScope{
		TenantID:       1,
		SourceTenantID: 1,
		SessionID:      "session-1",
		SourceIDs:      []string{src.ID},
	}
	if _, err := svc.Schema(ctx, scope, SchemaInput{TableNames: []string{orders.VirtualName}}); err != nil {
		t.Fatalf("schema orders table: %v", err)
	}

	_, err := svc.ExecuteQuery(ctx, scope, QueryInput{SQL: "SELECT COUNT(*) AS n FROM customers_v"}, true)
	if err == nil || !strings.Contains(err.Error(), "db_schema") || !strings.Contains(err.Error(), customers.VirtualName) {
		t.Fatalf("ExecuteQuery error = %v, want referenced table schema requirement", err)
	}
}
