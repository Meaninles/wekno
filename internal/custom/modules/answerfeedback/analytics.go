package answerfeedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	analyticsDefaultPageSize = 20
	analyticsMaxPageSize     = 100
	analyticsMaxOptionSize   = 100
)

var ErrAnalyticsFeedbackNotFound = errors.New("feedback analytics record not found")

type analyticsResourceKey struct {
	ID       string
	TenantID uint64
}

type AnalyticsQuery struct {
	Page               int
	PageSize           int
	UserID             string
	UserQuery          string
	AgentKeys          []analyticsResourceKey
	AgentQuery         string
	KnowledgeBaseKeys  []analyticsResourceKey
	KnowledgeBaseQuery string
	Feedback           string
	Channel            string
	From               *time.Time
	To                 *time.Time
}

type FeedbackAnalyticsResourceOption struct {
	Key       string "json:\"key\""
	ID        string "json:\"id\""
	Name      string "json:\"name\""
	TenantID  uint64 "json:\"tenant_id\""
	IsBuiltin bool   "json:\"is_builtin,omitempty\""
	IsDeleted bool   "json:\"is_deleted,omitempty\""
}

type FeedbackAnalyticsResource struct {
	ID        string "json:\"id\""
	Name      string "json:\"name\""
	TenantID  uint64 "json:\"tenant_id,omitempty\""
	IsBuiltin bool   "json:\"is_builtin,omitempty\""
	IsDeleted bool   "json:\"is_deleted,omitempty\""
}

type FeedbackAnalyticsItem struct {
	ID                  string                      "json:\"id\""
	TenantID            uint64                      "json:\"tenant_id\""
	TenantName          string                      "json:\"tenant_name,omitempty\""
	UserID              string                      "json:\"user_id,omitempty\""
	UserName            string                      "json:\"user_name,omitempty\""
	SessionID           string                      "json:\"session_id\""
	SessionTitle        string                      "json:\"session_title,omitempty\""
	RequestID           string                      "json:\"request_id,omitempty\""
	UserMessageID       string                      "json:\"user_message_id,omitempty\""
	AssistantMessageID  string                      "json:\"assistant_message_id\""
	Feedback            string                      "json:\"feedback\""
	Channel             string                      "json:\"channel,omitempty\""
	Question            string                      "json:\"question,omitempty\""
	Answer              string                      "json:\"answer,omitempty\""
	KnowledgeReferences types.References            "json:\"knowledge_references,omitempty\""
	Agent               *FeedbackAnalyticsResource  "json:\"agent,omitempty\""
	KnowledgeBases      []FeedbackAnalyticsResource "json:\"knowledge_bases,omitempty\""
	CreatedAt           time.Time                   "json:\"created_at\""
	UpdatedAt           time.Time                   "json:\"updated_at\""
	AnswerCreatedAt     time.Time                   "json:\"answer_created_at,omitempty\""
	HasSnapshot         bool                        "json:\"has_snapshot\""
}

type FeedbackAnalyticsSummary struct {
	Total      int64 "json:\"total\""
	Solved     int64 "json:\"solved\""
	OffTopic   int64 "json:\"off_topic\""
	Inaccurate int64 "json:\"inaccurate\""
	Unsolved   int64 "json:\"unsolved\""
}

type FeedbackAnalyticsPage struct {
	Items    []FeedbackAnalyticsItem  "json:\"items\""
	Total    int64                    "json:\"total\""
	Page     int                      "json:\"page\""
	PageSize int                      "json:\"page_size\""
	Summary  FeedbackAnalyticsSummary "json:\"summary\""
}

type FeedbackAnalyticsGroup struct {
	Key           string "json:\"key\""
	ID            string "json:\"id\""
	TenantID      uint64 "json:\"tenant_id,omitempty\""
	Name          string "json:\"name\""
	FeedbackCount int64  "json:\"feedback_count\""
	Solved        int64  "json:\"solved\""
	OffTopic      int64  "json:\"off_topic\""
	Inaccurate    int64  "json:\"inaccurate\""
	Unsolved      int64  "json:\"unsolved\""
}

type FeedbackAnalyticsConversationMessage struct {
	ID                  string           "json:\"id\""
	RequestID           string           "json:\"request_id,omitempty\""
	Role                string           "json:\"role\""
	Content             string           "json:\"content\""
	KnowledgeReferences types.References "json:\"knowledge_references,omitempty\""
	ErrorCode           string           "json:\"error_code,omitempty\""
	IsCompleted         bool             "json:\"is_completed\""
	Channel             string           "json:\"channel,omitempty\""
	CreatedAt           time.Time        "json:\"created_at\""
}

type FeedbackAnalyticsConversationPage struct {
	Items    []FeedbackAnalyticsConversationMessage "json:\"items\""
	Total    int64                                  "json:\"total\""
	Page     int                                    "json:\"page\""
	PageSize int                                    "json:\"page_size\""
}

type FeedbackAnalyticsDetail struct {
	Feedback     FeedbackAnalyticsItem             "json:\"feedback\""
	Conversation FeedbackAnalyticsConversationPage "json:\"conversation\""
}

type analyticsScope struct {
	system        bool
	tenantID      uint64
	userID        string
	agentKeys     map[string]analyticsResourceKey
	knowledgeKeys map[string]analyticsResourceKey
}

func (scope *analyticsScope) addAgent(key analyticsResourceKey) {
	if scope.agentKeys == nil {
		scope.agentKeys = make(map[string]analyticsResourceKey)
	}
	key.ID = strings.TrimSpace(key.ID)
	if key.ID == "" || key.TenantID == 0 {
		return
	}
	scope.agentKeys[analyticsResourceKeyString(key)] = key
}

func (scope *analyticsScope) addKnowledgeBase(key analyticsResourceKey) {
	if scope.knowledgeKeys == nil {
		scope.knowledgeKeys = make(map[string]analyticsResourceKey)
	}
	key.ID = strings.TrimSpace(key.ID)
	if key.ID == "" || key.TenantID == 0 {
		return
	}
	scope.knowledgeKeys[analyticsResourceKeyString(key)] = key
}

func analyticsResourceKeyString(key analyticsResourceKey) string {
	return strconv.FormatUint(key.TenantID, 10) + ":" + key.ID
}

func (scope analyticsScope) agentKeySlice() []analyticsResourceKey {
	items := make([]analyticsResourceKey, 0, len(scope.agentKeys))
	for _, key := range scope.agentKeys {
		items = append(items, key)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].TenantID != items[j].TenantID {
			return items[i].TenantID < items[j].TenantID
		}
		return items[i].ID < items[j].ID
	})
	return items
}

func (scope analyticsScope) knowledgeKeySlice() []analyticsResourceKey {
	items := make([]analyticsResourceKey, 0, len(scope.knowledgeKeys))
	for _, key := range scope.knowledgeKeys {
		items = append(items, key)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].TenantID != items[j].TenantID {
			return items[i].TenantID < items[j].TenantID
		}
		return items[i].ID < items[j].ID
	})
	return items
}

func (scope analyticsScope) allowsAgent(id string, tenantID uint64) bool {
	if scope.system {
		return true
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if tenantID != 0 {
		_, ok := scope.agentKeys[analyticsResourceKeyString(analyticsResourceKey{ID: id, TenantID: tenantID})]
		return ok
	}
	for _, key := range scope.agentKeys {
		if key.ID == id {
			return true
		}
	}
	return false
}

func (scope analyticsScope) allowsKnowledgeBase(id string) bool {
	if scope.system {
		return true
	}
	for _, key := range scope.knowledgeKeys {
		if key.ID == id {
			return true
		}
	}
	return false
}

type analyticsSnapshotPayload struct {
	Agent struct {
		ID        string "json:\"id\""
		Name      string "json:\"name\""
		TenantID  uint64 "json:\"tenant_id\""
		IsBuiltin bool   "json:\"is_builtin\""
	} "json:\"agent\""
	Request struct {
		UserQuery        string   "json:\"user_query\""
		Channel          string   "json:\"channel\""
		KnowledgeBaseIDs []string "json:\"knowledge_base_ids\""
	} "json:\"request\""
	KnowledgeReferences types.References "json:\"knowledge_references\""
}

type analyticsRecordRow struct {
	FeedbackID                   string
	TenantID                     uint64
	TenantName                   string
	UserID                       string
	UserName                     string
	SessionID                    string
	SessionTitle                 string
	RequestID                    string
	UserMessageID                string
	AssistantMessageID           string
	Feedback                     string
	Channel                      string
	UserQuery                    string
	AssistantAnswer              string
	AgentID                      string
	AgentTenantID                uint64
	AgentName                    string
	Snapshot                     types.JSON
	AssistantKnowledgeReferences types.References
	FeedbackCreatedAt            time.Time
	FeedbackUpdatedAt            time.Time
	AnswerCreatedAt              time.Time
}

type analyticsOptionRow struct {
	ID        string
	Name      string
	TenantID  uint64
	IsBuiltin bool
	IsDeleted bool
}

type analyticsSummaryRow struct {
	Total      int64
	Solved     int64
	OffTopic   int64
	Inaccurate int64
	Unsolved   int64
}

type analyticsGroupRow struct {
	ID            string "gorm:\"column:resource_id\""
	TenantID      uint64 "gorm:\"column:resource_tenant_id\""
	Name          string "gorm:\"column:resource_name\""
	FeedbackCount int64  "gorm:\"column:feedback_count\""
	Solved        int64  "gorm:\"column:solved\""
	OffTopic      int64  "gorm:\"column:off_topic\""
	Inaccurate    int64  "gorm:\"column:inaccurate\""
	Unsolved      int64  "gorm:\"column:unsolved\""
}

func (s *Service) loadAnalyticsScope(ctx context.Context) (analyticsScope, error) {
	scope := analyticsScope{
		system:        types.IsSystemAdminFromContext(ctx),
		agentKeys:     make(map[string]analyticsResourceKey),
		knowledgeKeys: make(map[string]analyticsResourceKey),
	}
	scope.tenantID, _ = types.TenantIDFromContext(ctx)
	scope.userID, _ = types.UserIDFromContext(ctx)
	if scope.system || s == nil || s.db == nil || scope.tenantID == 0 {
		return scope, nil
	}

	var tenantAgents []analyticsResourceKey
	if err := s.db.WithContext(ctx).Unscoped().Table("custom_agents").
		Select("id, tenant_id").
		Where("tenant_id = ?", scope.tenantID).
		Scan(&tenantAgents).Error; err != nil {
		return scope, err
	}
	for _, key := range tenantAgents {
		scope.addAgent(key)
	}
	for _, id := range types.GetBuiltinAgentIDs() {
		scope.addAgent(analyticsResourceKey{ID: id, TenantID: scope.tenantID})
	}

	var sharedAgents []analyticsResourceKey
	if err := s.db.WithContext(ctx).Table("agent_shares AS sh").
		Select("sh.agent_id AS id, sh.source_tenant_id AS tenant_id").
		Joins("JOIN organization_tenant_members AS om ON om.organization_id = sh.organization_id AND om.tenant_id = ? AND om.role = ?",
			scope.tenantID, types.OrgRoleAdmin).
		Where("sh.permission = ? AND sh.deleted_at IS NULL", types.OrgRoleAdmin).
		Scan(&sharedAgents).Error; err != nil {
		return scope, err
	}
	for _, key := range sharedAgents {
		scope.addAgent(key)
	}

	var tenantKnowledgeBases []analyticsResourceKey
	if err := s.db.WithContext(ctx).Unscoped().Table("knowledge_bases").
		Select("id, tenant_id").
		Where("tenant_id = ?", scope.tenantID).
		Scan(&tenantKnowledgeBases).Error; err != nil {
		return scope, err
	}
	for _, key := range tenantKnowledgeBases {
		scope.addKnowledgeBase(key)
	}

	var sharedKnowledgeBases []analyticsResourceKey
	if err := s.db.WithContext(ctx).Table("kb_shares AS sh").
		Select("sh.knowledge_base_id AS id, sh.source_tenant_id AS tenant_id").
		Joins("JOIN organization_tenant_members AS om ON om.organization_id = sh.organization_id AND om.tenant_id = ? AND om.role = ?",
			scope.tenantID, types.OrgRoleAdmin).
		Where("sh.permission = ? AND sh.deleted_at IS NULL", types.OrgRoleAdmin).
		Scan(&sharedKnowledgeBases).Error; err != nil {
		return scope, err
	}
	for _, key := range sharedKnowledgeBases {
		scope.addKnowledgeBase(key)
	}
	return scope, nil
}

func analyticsResourcePredicate(alias string, keys []analyticsResourceKey) (string, []any) {
	return analyticsResourcePredicateColumns(alias+".id", alias+".tenant_id", keys)
}

func analyticsSnapshotAgentPredicate(keys []analyticsResourceKey) (string, []any) {
	return analyticsResourcePredicateColumns("rs.agent_id", "rs.agent_tenant_id", keys)
}

func analyticsResourcePredicateColumns(idExpression, tenantExpression string, keys []analyticsResourceKey) (string, []any) {
	if len(keys) == 0 {
		return "1 = 0", nil
	}
	parts := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)*2)
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		key.ID = strings.TrimSpace(key.ID)
		if key.ID == "" || key.TenantID == 0 {
			continue
		}
		keyString := analyticsResourceKeyString(key)
		if seen[keyString] {
			continue
		}
		seen[keyString] = true
		parts = append(parts, fmt.Sprintf("(%s = ? AND %s = ?)", idExpression, tenantExpression))
		args = append(args, key.ID, key.TenantID)
	}
	if len(parts) == 0 {
		return "1 = 0", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func analyticsKnowledgePredicate(ids []string) (string, []any) {
	parts := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	knowledgeIDs := analyticsKnowledgeIDsSubquery()
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		parts = append(parts, "EXISTS (SELECT 1 FROM ("+knowledgeIDs+") AS feedback_kb(id) WHERE feedback_kb.id = ?)")
		args = append(args, id)
	}
	if len(parts) == 0 {
		return "1 = 0", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func analyticsJSONArrayExpression(path string) string {
	return "CASE WHEN jsonb_typeof(" + path + ") = 'array' THEN " + path + " ELSE '[]'::jsonb END"
}

func analyticsKnowledgeIDsSubquery() string {
	requestPath := "rs.snapshot #> '{request,knowledge_base_ids}'"
	referencesPath := "rs.snapshot #> '{knowledge_references}'"
	return "SELECT jsonb_array_elements_text(" + analyticsJSONArrayExpression(requestPath) + ") AS id " +
		"UNION SELECT reference.value->>'knowledge_base_id' AS id " +
		"FROM jsonb_array_elements(" + analyticsJSONArrayExpression(referencesPath) + ") AS reference(value) " +
		"WHERE NULLIF(reference.value->>'knowledge_base_id', '') IS NOT NULL"
}

func applyAnalyticsScope(db *gorm.DB, scope analyticsScope) *gorm.DB {
	if scope.system {
		return db
	}
	agentPredicate, agentArgs := analyticsSnapshotAgentPredicate(scope.agentKeySlice())
	kbIDs := make([]string, 0, len(scope.knowledgeKeys))
	for _, key := range scope.knowledgeKeys {
		kbIDs = append(kbIDs, key.ID)
	}
	kbPredicate, kbArgs := analyticsKnowledgePredicate(kbIDs)
	return db.Where("("+agentPredicate+" OR "+kbPredicate+")", append(agentArgs, kbArgs...)...)
}

func applyAnalyticsFilters(db *gorm.DB, query AnalyticsQuery) *gorm.DB {
	if query.UserID != "" {
		db = db.Where("f.user_id = ?", query.UserID)
	}
	if query.UserQuery != "" {
		pattern := "%" + analyticsEscapeLike(strings.ToLower(query.UserQuery)) + "%"
		db = db.Where(
			"(LOWER(COALESCE(u.username, '')) LIKE ? ESCAPE '!' OR LOWER(COALESCE(u.display_name, '')) LIKE ? ESCAPE '!' OR LOWER(COALESCE(u.id, '')) LIKE ? ESCAPE '!' OR LOWER(COALESCE(f.user_id, '')) LIKE ? ESCAPE '!' OR LOWER(COALESCE(t.name, '')) LIKE ? ESCAPE '!')",
			pattern, pattern, pattern, pattern, pattern,
		)
	}
	if len(query.AgentKeys) > 0 {
		predicate, args := analyticsSnapshotAgentPredicate(query.AgentKeys)
		db = db.Where(predicate, args...)
	}
	if query.AgentQuery != "" {
		pattern := "%" + analyticsEscapeLike(strings.ToLower(query.AgentQuery)) + "%"
		db = db.Where(
			"(LOWER(COALESCE(a.name, '')) LIKE ? ESCAPE '!' OR LOWER(COALESCE(rs.snapshot #>> '{agent,name}', '')) LIKE ? ESCAPE '!' OR LOWER(COALESCE(rs.agent_id, '')) LIKE ? ESCAPE '!')",
			pattern, pattern, pattern,
		)
	}
	if len(query.KnowledgeBaseKeys) > 0 {
		ids := make([]string, 0, len(query.KnowledgeBaseKeys))
		for _, key := range query.KnowledgeBaseKeys {
			ids = append(ids, key.ID)
		}
		predicate, args := analyticsKnowledgePredicate(ids)
		db = db.Where(predicate, args...)
	}
	if query.KnowledgeBaseQuery != "" {
		pattern := "%" + analyticsEscapeLike(strings.ToLower(query.KnowledgeBaseQuery)) + "%"
		db = db.Where(
			"EXISTS (SELECT 1 FROM knowledge_bases AS filter_kb WHERE filter_kb.id IN ("+analyticsKnowledgeIDsSubquery()+") AND filter_kb.deleted_at IS NULL AND LOWER(COALESCE(filter_kb.name, '')) LIKE ? ESCAPE '!')",
			pattern,
		)
	}
	if query.Feedback != "" {
		db = db.Where("f.feedback = ?", query.Feedback)
	}
	if query.Channel != "" {
		db = db.Where("f.channel = ?", query.Channel)
	}
	if query.From != nil {
		db = db.Where("f.updated_at >= ?", *query.From)
	}
	if query.To != nil {
		db = db.Where("f.updated_at < ?", *query.To)
	}
	return db
}

func (s *Service) analyticsBase(ctx context.Context, query AnalyticsQuery) (*gorm.DB, analyticsScope, error) {
	scope, err := s.loadAnalyticsScope(ctx)
	if err != nil {
		return nil, scope, err
	}
	db := s.db.WithContext(ctx).
		Table("custom_answer_feedbacks AS f").
		Joins("LEFT JOIN custom_answer_run_snapshots AS rs ON rs.assistant_message_id = f.assistant_message_id AND rs.tenant_id = f.tenant_id").
		Joins("LEFT JOIN sessions AS se ON se.id = f.session_id AND se.tenant_id = f.tenant_id AND se.deleted_at IS NULL").
		Joins("LEFT JOIN users AS u ON u.id = f.user_id AND u.deleted_at IS NULL").
		Joins("LEFT JOIN tenants AS t ON t.id = f.tenant_id AND t.deleted_at IS NULL").
		Joins("LEFT JOIN messages AS am ON am.id = f.assistant_message_id AND am.session_id = f.session_id AND am.role = 'assistant' AND am.deleted_at IS NULL").
		Joins("LEFT JOIN custom_agents AS a ON a.id = rs.agent_id AND a.tenant_id = rs.agent_tenant_id AND a.deleted_at IS NULL").
		Joins("LEFT JOIN LATERAL (" +
			"SELECT m.id, m.content, m.created_at FROM messages AS m " +
			"WHERE m.session_id = f.session_id AND m.role = 'user' AND m.deleted_at IS NULL " +
			"AND ((NULLIF(rs.user_message_id, '') IS NOT NULL AND m.id = rs.user_message_id) " +
			"OR (NULLIF(rs.user_message_id, '') IS NULL AND f.request_id <> '' AND m.request_id = f.request_id)) " +
			"ORDER BY m.created_at DESC, m.id DESC LIMIT 1" +
			") AS um ON TRUE")
	if !scope.system {
		db = applyAnalyticsScope(db, scope)
	}
	return applyAnalyticsFilters(db, query), scope, nil
}

func analyticsRecordSelect() string {
	return "f.id AS feedback_id, f.tenant_id, COALESCE(t.name, '') AS tenant_name, " +
		"COALESCE(f.user_id, '') AS user_id, " +
		"COALESCE(NULLIF(u.display_name, ''), NULLIF(u.username, ''), '') AS user_name, " +
		"f.session_id, COALESCE(se.title, '') AS session_title, f.request_id, " +
		"COALESCE(NULLIF(rs.user_message_id, ''), NULLIF(um.id, ''), '') AS user_message_id, " +
		"f.assistant_message_id, f.feedback, f.channel, " +
		"COALESCE(NULLIF(rs.user_query, ''), NULLIF(um.content, ''), '') AS user_query, " +
		"COALESCE(NULLIF(rs.assistant_answer, ''), NULLIF(am.content, ''), '') AS assistant_answer, " +
		"COALESCE(rs.agent_id, '') AS agent_id, COALESCE(rs.agent_tenant_id, 0) AS agent_tenant_id, " +
		"COALESCE(NULLIF(a.name, ''), NULLIF(rs.snapshot #>> '{agent,name}', ''), '') AS agent_name, " +
		"rs.snapshot, am.knowledge_references AS assistant_knowledge_references, " +
		"f.created_at AS feedback_created_at, f.updated_at AS feedback_updated_at, " +
		"am.created_at AS answer_created_at"
}

func (s *Service) ListAnalytics(ctx context.Context, query AnalyticsQuery) (*FeedbackAnalyticsPage, error) {
	query = normalizeAnalyticsQuery(query)
	base, scope, err := s.analyticsBase(ctx, query)
	if err != nil {
		return nil, err
	}
	var totalRow struct {
		Total int64
	}
	// GORM mutates the statement while building the FROM clause. Keep every
	// query on its own session; reusing base after Scan would append the raw
	// joins a second time (notably the rs alias) and make PostgreSQL reject it.
	if err := base.Session(&gorm.Session{}).Select("COUNT(*) AS total").Scan(&totalRow).Error; err != nil {
		return nil, err
	}
	var rows []analyticsRecordRow
	if err := base.Session(&gorm.Session{}).Select(analyticsRecordSelect()).
		Order("f.updated_at DESC, f.id DESC").
		Limit(query.PageSize).Offset((query.Page - 1) * query.PageSize).Scan(&rows).Error; err != nil {
		return nil, err
	}
	items, err := s.enrichAnalyticsRows(ctx, rows, scope, false)
	if err != nil {
		return nil, err
	}
	summary, err := scanAnalyticsSummary(base.Session(&gorm.Session{}))
	if err != nil {
		return nil, err
	}
	summary.Total = totalRow.Total
	return &FeedbackAnalyticsPage{
		Items: items, Total: totalRow.Total, Page: query.Page,
		PageSize: query.PageSize, Summary: summary,
	}, nil
}

func scanAnalyticsSummary(base *gorm.DB) (FeedbackAnalyticsSummary, error) {
	var row analyticsSummaryRow
	err := base.Select(
		"COUNT(*) AS total, "+
			"COALESCE(SUM(CASE WHEN f.feedback = ? THEN 1 ELSE 0 END), 0) AS solved, "+
			"COALESCE(SUM(CASE WHEN f.feedback = ? THEN 1 ELSE 0 END), 0) AS off_topic, "+
			"COALESCE(SUM(CASE WHEN f.feedback = ? THEN 1 ELSE 0 END), 0) AS inaccurate, "+
			"COALESCE(SUM(CASE WHEN f.feedback = ? THEN 1 ELSE 0 END), 0) AS unsolved",
		FeedbackSolved, FeedbackOffTopic, FeedbackInaccurate, FeedbackUnsolved,
	).Scan(&row).Error
	return FeedbackAnalyticsSummary{
		Total: row.Total, Solved: row.Solved, OffTopic: row.OffTopic,
		Inaccurate: row.Inaccurate, Unsolved: row.Unsolved,
	}, err
}

func (s *Service) enrichAnalyticsRows(ctx context.Context, rows []analyticsRecordRow, scope analyticsScope, includeReferences bool) ([]FeedbackAnalyticsItem, error) {
	if len(rows) == 0 {
		return []FeedbackAnalyticsItem{}, nil
	}
	payloads := make([]analyticsSnapshotPayload, len(rows))
	kbIDs := make([]string, 0)
	kbSeen := make(map[string]bool)
	for i := range rows {
		if len(rows[i].Snapshot) > 0 {
			_ = json.Unmarshal([]byte(rows[i].Snapshot), &payloads[i])
		}
		for _, id := range analyticsSnapshotKnowledgeBaseIDs(payloads[i], rows[i].AssistantKnowledgeReferences) {
			id = strings.TrimSpace(id)
			if id != "" && !kbSeen[id] {
				kbSeen[id] = true
				kbIDs = append(kbIDs, id)
			}
		}
	}
	type knowledgeBaseRow struct {
		ID        string
		Name      string
		TenantID  uint64
		IsDeleted bool
	}
	kbMap := make(map[string]knowledgeBaseRow, len(kbIDs))
	if len(kbIDs) > 0 {
		var knowledgeBases []knowledgeBaseRow
		if err := s.db.WithContext(ctx).Unscoped().Table("knowledge_bases").
			Select("id, name, tenant_id, (deleted_at IS NOT NULL) AS is_deleted").
			Where("id IN ?", kbIDs).Find(&knowledgeBases).Error; err != nil {
			return nil, err
		}
		for _, row := range knowledgeBases {
			if _, exists := kbMap[row.ID]; !exists {
				kbMap[row.ID] = row
			}
		}
	}
	items := make([]FeedbackAnalyticsItem, 0, len(rows))
	for i, row := range rows {
		payload := payloads[i]
		references := payload.KnowledgeReferences
		if len(references) == 0 {
			references = row.AssistantKnowledgeReferences
		}
		agentID := strings.TrimSpace(row.AgentID)
		if agentID == "" {
			agentID = strings.TrimSpace(payload.Agent.ID)
		}
		agentTenantID := row.AgentTenantID
		if agentTenantID == 0 {
			agentTenantID = payload.Agent.TenantID
		}
		if agentTenantID == 0 {
			agentTenantID = row.TenantID
		}
		agentName := strings.TrimSpace(row.AgentName)
		if agentName == "" {
			agentName = strings.TrimSpace(payload.Agent.Name)
		}
		isBuiltin := payload.Agent.IsBuiltin || strings.HasPrefix(agentID, "builtin-") || types.IsBuiltinAgentID(agentID)
		if agentName == "" && isBuiltin {
			if builtin := types.GetBuiltinAgent(agentID, agentTenantID); builtin != nil {
				agentName = builtin.Name
			}
		}
		if agentName == "" {
			agentName = agentID
		}
		item := FeedbackAnalyticsItem{
			ID: row.FeedbackID, TenantID: row.TenantID, TenantName: row.TenantName,
			UserID: row.UserID, UserName: row.UserName, SessionID: row.SessionID,
			SessionTitle: row.SessionTitle, RequestID: row.RequestID,
			UserMessageID: row.UserMessageID, AssistantMessageID: row.AssistantMessageID,
			Feedback: row.Feedback, Channel: row.Channel, Question: row.UserQuery,
			Answer: row.AssistantAnswer, CreatedAt: row.FeedbackCreatedAt,
			UpdatedAt: row.FeedbackUpdatedAt, AnswerCreatedAt: row.AnswerCreatedAt,
			HasSnapshot: len(row.Snapshot) > 0, KnowledgeBases: []FeedbackAnalyticsResource{},
		}
		if includeReferences {
			// Keep the immutable reference snapshot intact for the detail view.
			// The web chat renderer needs the full list to resolve every <src>
			// marker and open the exact cited source.
			item.KnowledgeReferences = references
		}
		if item.Question == "" {
			item.Question = payload.Request.UserQuery
		}
		if item.Channel == "" {
			item.Channel = payload.Request.Channel
		}
		if !scope.system && !scope.allowsAgent(agentID, agentTenantID) {
			agentID = ""
		}
		if agentID != "" {
			item.Agent = &FeedbackAnalyticsResource{
				ID: agentID, Name: agentName, TenantID: agentTenantID, IsBuiltin: isBuiltin,
			}
		}
		for _, id := range analyticsSnapshotKnowledgeBaseIDs(payload, references) {
			id = strings.TrimSpace(id)
			if id == "" || (!scope.system && !scope.allowsKnowledgeBase(id)) {
				continue
			}
			resource := FeedbackAnalyticsResource{ID: id, Name: id}
			if kb, ok := kbMap[id]; ok {
				resource.Name = firstNonEmpty(strings.TrimSpace(kb.Name), id)
				resource.TenantID = kb.TenantID
				resource.IsDeleted = kb.IsDeleted
			}
			item.KnowledgeBases = append(item.KnowledgeBases, resource)
		}
		items = append(items, item)
	}
	return items, nil
}

func analyticsSnapshotKnowledgeBaseIDs(payload analyticsSnapshotPayload, fallbackReferences types.References) []string {
	references := payload.KnowledgeReferences
	if len(references) == 0 {
		references = fallbackReferences
	}
	ids := make([]string, 0, len(payload.Request.KnowledgeBaseIDs)+len(references))
	seen := make(map[string]bool, cap(ids))
	for _, id := range payload.Request.KnowledgeBaseIDs {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, reference := range references {
		if reference == nil {
			continue
		}
		id := strings.TrimSpace(reference.KnowledgeBaseID)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func normalizeAnalyticsQuery(query AnalyticsQuery) AnalyticsQuery {
	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 {
		query.PageSize = analyticsDefaultPageSize
	}
	if query.PageSize > analyticsMaxPageSize {
		query.PageSize = analyticsMaxPageSize
	}
	query.UserID = strings.TrimSpace(query.UserID)
	query.UserQuery = strings.TrimSpace(query.UserQuery)
	query.AgentQuery = strings.TrimSpace(query.AgentQuery)
	query.KnowledgeBaseQuery = strings.TrimSpace(query.KnowledgeBaseQuery)
	query.Feedback = strings.TrimSpace(query.Feedback)
	query.Channel = strings.TrimSpace(query.Channel)
	return query
}

func analyticsEscapeLike(value string) string {
	value = strings.ReplaceAll(value, "!", "!!")
	value = strings.ReplaceAll(value, "%", "!%")
	return strings.ReplaceAll(value, "_", "!_")
}

func (s *Service) ListAnalyticsResourceOptions(ctx context.Context, kind, query string, limit int) ([]FeedbackAnalyticsResourceOption, error) {
	kind = strings.TrimSpace(kind)
	if kind != "agents" && kind != "knowledge_bases" {
		return nil, errors.New("unsupported analytics resource kind")
	}
	if limit <= 0 || limit > analyticsMaxOptionSize {
		limit = analyticsMaxOptionSize
	}
	scope, err := s.loadAnalyticsScope(ctx)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	pattern := "%" + analyticsEscapeLike(query) + "%"
	rows := make([]analyticsOptionRow, 0)
	if kind == "agents" {
		db := s.db.WithContext(ctx).Unscoped().Table("custom_agents AS a")
		if !scope.system {
			predicate, args := analyticsResourcePredicate("a", scope.agentKeySlice())
			db = db.Where(predicate, args...)
		}
		if query != "" {
			db = db.Where("(LOWER(COALESCE(a.name, '')) LIKE ? ESCAPE '!' OR LOWER(a.id) LIKE ? ESCAPE '!')", pattern, pattern)
		}
		if err := db.Select("a.id, a.name, a.tenant_id, a.is_builtin, (a.deleted_at IS NOT NULL) AS is_deleted").
			Order("LOWER(COALESCE(a.name, a.id)), a.tenant_id, a.id").Limit(limit).Scan(&rows).Error; err != nil {
			return nil, err
		}
	} else {
		db := s.db.WithContext(ctx).Unscoped().Table("knowledge_bases AS kb")
		if !scope.system {
			predicate, args := analyticsResourcePredicate("kb", scope.knowledgeKeySlice())
			db = db.Where(predicate, args...)
		}
		if query != "" {
			db = db.Where("(LOWER(COALESCE(kb.name, '')) LIKE ? ESCAPE '!' OR LOWER(kb.id) LIKE ? ESCAPE '!')", pattern, pattern)
		}
		if err := db.Select("kb.id, kb.name, kb.tenant_id, FALSE AS is_builtin, (kb.deleted_at IS NOT NULL) AS is_deleted").
			Order("LOWER(COALESCE(kb.name, kb.id)), kb.tenant_id, kb.id").Limit(limit).Scan(&rows).Error; err != nil {
			return nil, err
		}
	}
	options := make([]FeedbackAnalyticsResourceOption, 0, len(rows)+len(types.GetBuiltinAgentIDs()))
	seen := make(map[string]bool)
	for _, row := range rows {
		key := analyticsResourceKeyString(analyticsResourceKey{ID: row.ID, TenantID: row.TenantID})
		seen[key] = true
		options = append(options, FeedbackAnalyticsResourceOption{
			Key: key, ID: row.ID, Name: firstNonEmpty(strings.TrimSpace(row.Name), row.ID),
			TenantID: row.TenantID, IsBuiltin: row.IsBuiltin, IsDeleted: row.IsDeleted,
		})
	}
	if kind == "agents" && !scope.system {
		for _, id := range types.GetBuiltinAgentIDs() {
			key := analyticsResourceKey{ID: id, TenantID: scope.tenantID}
			keyString := analyticsResourceKeyString(key)
			if seen[keyString] {
				continue
			}
			name := id
			if builtin := types.GetBuiltinAgent(id, scope.tenantID); builtin != nil {
				name = firstNonEmpty(builtin.Name, id)
			}
			if query != "" && !strings.Contains(strings.ToLower(name), query) && !strings.Contains(strings.ToLower(id), query) {
				continue
			}
			options = append(options, FeedbackAnalyticsResourceOption{
				Key: keyString, ID: id, Name: name, TenantID: scope.tenantID, IsBuiltin: true,
			})
		}
	}
	sort.Slice(options, func(i, j int) bool {
		if strings.ToLower(options[i].Name) != strings.ToLower(options[j].Name) {
			return strings.ToLower(options[i].Name) < strings.ToLower(options[j].Name)
		}
		return options[i].Key < options[j].Key
	})
	if len(options) > limit {
		options = options[:limit]
	}
	return options, nil
}

func (s *Service) ListAnalyticsGroups(ctx context.Context, query AnalyticsQuery, dimension string) ([]FeedbackAnalyticsGroup, error) {
	dimension = strings.TrimSpace(dimension)
	if dimension != "agent" && dimension != "knowledge_base" {
		return nil, errors.New("unsupported analytics group dimension")
	}
	base, scope, err := s.analyticsBase(ctx, normalizeAnalyticsQuery(query))
	if err != nil {
		return nil, err
	}
	var rows []analyticsGroupRow
	if dimension == "agent" {
		if !scope.system {
			predicate, args := analyticsSnapshotAgentPredicate(scope.agentKeySlice())
			base = base.Where(predicate, args...)
		}
		base = base.Where("NULLIF(rs.agent_id, '') IS NOT NULL")
		nameExpr := "COALESCE(NULLIF(a.name, ''), NULLIF(rs.snapshot #>> '{agent,name}', ''), NULLIF(rs.agent_id, ''), '未命名智能体')"
		err = base.Select(
			"rs.agent_id AS resource_id, rs.agent_tenant_id AS resource_tenant_id, "+nameExpr+" AS resource_name, "+
				"COUNT(DISTINCT f.id) AS feedback_count, "+
				"COUNT(DISTINCT CASE WHEN f.feedback = ? THEN f.id END) AS solved, "+
				"COUNT(DISTINCT CASE WHEN f.feedback = ? THEN f.id END) AS off_topic, "+
				"COUNT(DISTINCT CASE WHEN f.feedback = ? THEN f.id END) AS inaccurate, "+
				"COUNT(DISTINCT CASE WHEN f.feedback = ? THEN f.id END) AS unsolved",
			FeedbackSolved, FeedbackOffTopic, FeedbackInaccurate, FeedbackUnsolved,
		).Group("rs.agent_id, rs.agent_tenant_id, a.name, rs.snapshot").
			Order("feedback_count DESC, resource_name ASC").Limit(200).Scan(&rows).Error
	} else {
		var allowedKnowledgeBaseIDs []string
		if !scope.system {
			ids := make([]string, 0, len(scope.knowledgeKeys))
			for _, key := range scope.knowledgeKeys {
				ids = append(ids, key.ID)
			}
			allowedKnowledgeBaseIDs = ids
			predicate, args := analyticsKnowledgePredicate(ids)
			base = base.Where(predicate, args...)
		}
		base = base.
			Joins("JOIN LATERAL (" + analyticsKnowledgeIDsSubquery() + ") AS gak(id) ON TRUE").
			Joins("LEFT JOIN knowledge_bases AS gkb ON gkb.id = gak.id AND gkb.deleted_at IS NULL")
		if !scope.system {
			base = base.Where("gak.id IN ?", allowedKnowledgeBaseIDs)
		}
		nameExpr := "COALESCE(NULLIF(gkb.name, ''), gak.id)"
		err = base.Select(
			"gak.id AS resource_id, COALESCE(gkb.tenant_id, 0) AS resource_tenant_id, "+nameExpr+" AS resource_name, "+
				"COUNT(DISTINCT f.id) AS feedback_count, "+
				"COUNT(DISTINCT CASE WHEN f.feedback = ? THEN f.id END) AS solved, "+
				"COUNT(DISTINCT CASE WHEN f.feedback = ? THEN f.id END) AS off_topic, "+
				"COUNT(DISTINCT CASE WHEN f.feedback = ? THEN f.id END) AS inaccurate, "+
				"COUNT(DISTINCT CASE WHEN f.feedback = ? THEN f.id END) AS unsolved",
			FeedbackSolved, FeedbackOffTopic, FeedbackInaccurate, FeedbackUnsolved,
		).Group("gak.id, gkb.id, gkb.name, gkb.tenant_id").
			Order("feedback_count DESC, resource_name ASC").Limit(200).Scan(&rows).Error
	}
	if err != nil {
		return nil, err
	}
	groups := make([]FeedbackAnalyticsGroup, 0, len(rows))
	for _, row := range rows {
		groups = append(groups, FeedbackAnalyticsGroup{
			Key: analyticsResourceKeyString(analyticsResourceKey{ID: row.ID, TenantID: row.TenantID}),
			ID:  row.ID, TenantID: row.TenantID, Name: firstNonEmpty(row.Name, row.ID),
			FeedbackCount: row.FeedbackCount, Solved: row.Solved, OffTopic: row.OffTopic,
			Inaccurate: row.Inaccurate, Unsolved: row.Unsolved,
		})
	}
	return groups, nil
}

func (s *Service) GetAnalyticsDetail(ctx context.Context, feedbackID string, page, pageSize int, includeConversation bool) (*FeedbackAnalyticsDetail, error) {
	feedbackID = strings.TrimSpace(feedbackID)
	if feedbackID == "" {
		return nil, ErrAnalyticsFeedbackNotFound
	}
	base, scope, err := s.analyticsBase(ctx, AnalyticsQuery{Page: 1, PageSize: 1})
	if err != nil {
		return nil, err
	}
	var row analyticsRecordRow
	result := base.Where("f.id = ?", feedbackID).Select(analyticsRecordSelect()).Limit(1).Scan(&row)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 || row.FeedbackID == "" {
		return nil, ErrAnalyticsFeedbackNotFound
	}
	items, err := s.enrichAnalyticsRows(ctx, []analyticsRecordRow{row}, scope, true)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrAnalyticsFeedbackNotFound
	}
	conversation := FeedbackAnalyticsConversationPage{
		Items:    []FeedbackAnalyticsConversationMessage{},
		Page:     page,
		PageSize: pageSize,
	}
	if includeConversation {
		conversation, err = s.listAnalyticsConversation(ctx, items[0].SessionID, items[0].AssistantMessageID, items[0].AnswerCreatedAt, page, pageSize)
		if err != nil {
			return nil, err
		}
	}
	return &FeedbackAnalyticsDetail{Feedback: items[0], Conversation: conversation}, nil
}

func normalizeAnalyticsPage(page int) int {
	if page <= 0 {
		return 1
	}
	return page
}

func normalizeAnalyticsPageSize(pageSize int) int {
	if pageSize <= 0 {
		return 50
	}
	if pageSize > analyticsMaxPageSize {
		return analyticsMaxPageSize
	}
	return pageSize
}

func (s *Service) listAnalyticsConversation(ctx context.Context, sessionID, assistantMessageID string, assistantCreatedAt time.Time, page, pageSize int) (FeedbackAnalyticsConversationPage, error) {
	page = normalizeAnalyticsPage(page)
	pageSize = normalizeAnalyticsPageSize(pageSize)
	db := s.db.WithContext(ctx).Table("messages AS m").
		Where("m.session_id = ? AND m.deleted_at IS NULL", sessionID)
	if !assistantCreatedAt.IsZero() {
		db = db.Where("(m.created_at < ? OR m.id = ?)", assistantCreatedAt, assistantMessageID)
	}
	var countRow struct {
		Total int64
	}
	if err := db.Session(&gorm.Session{}).Select("COUNT(*) AS total").Scan(&countRow).Error; err != nil {
		return FeedbackAnalyticsConversationPage{}, err
	}
	total := countRow.Total
	var messages []FeedbackAnalyticsConversationMessage
	offset := total - int64(page*pageSize)
	if offset < 0 {
		offset = 0
	}
	if err := db.Session(&gorm.Session{}).Select("m.id, m.request_id, m.role, m.content, m.knowledge_references, m.error_code, m.is_completed, m.channel, m.created_at").
		Order("m.created_at ASC, m.id ASC").Limit(pageSize).Offset(int(offset)).Scan(&messages).Error; err != nil {
		return FeedbackAnalyticsConversationPage{}, err
	}
	if messages == nil {
		messages = []FeedbackAnalyticsConversationMessage{}
	}
	return FeedbackAnalyticsConversationPage{Items: messages, Total: total, Page: page, PageSize: pageSize}, nil
}
