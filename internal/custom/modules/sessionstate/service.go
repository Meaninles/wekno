package sessionstate

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Tencent/WeKnora/internal/types"
)

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	db := s.db.Session(&gorm.Session{NewDB: true})
	config := *db.Config
	config.DisableForeignKeyConstraintWhenMigrating = true
	db.Config = &config
	return db.WithContext(ctx).AutoMigrate(&ReadState{})
}

func (s *Service) ListStatus(ctx context.Context, sessionIDs []string) (map[string]Status, error) {
	result := make(map[string]Status)
	if s == nil || s.db == nil {
		return result, nil
	}
	ids := normalizeIDs(sessionIDs)
	if len(ids) == 0 {
		return result, nil
	}

	visibleIDs, err := s.visibleSessionIDs(ctx, ids)
	if err != nil {
		return result, err
	}
	if len(visibleIDs) == 0 {
		return result, nil
	}
	for id := range visibleIDs {
		result[id] = Status{SessionID: id}
	}

	lastMessages, err := s.lastAssistantMessages(ctx, mapKeys(visibleIDs))
	if err != nil {
		return result, err
	}

	tenantID, _ := types.TenantIDFromContext(ctx)
	_, actorKey := actorKeyFromContext(ctx)
	readStates, err := s.readStates(ctx, tenantID, actorKey, mapKeys(visibleIDs))
	if err != nil {
		return result, err
	}

	for sessionID, msg := range lastMessages {
		at := msg.lastAt()
		status := result[sessionID]
		status.LastAssistantMessageID = msg.ID
		if !at.IsZero() {
			status.LastAssistantAt = &at
		}
		status.IsRunning = !msg.IsCompleted
		if msg.IsCompleted && !at.IsZero() {
			readAt, hasReadState := readStates[sessionID]
			status.Unread = hasReadState && readAt.Before(at)
		}
		result[sessionID] = status
	}

	return result, nil
}

func (s *Service) MarkRead(ctx context.Context, sessionID string) (Status, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Status{}, nil
	}
	visibleIDs, err := s.visibleSessionIDs(ctx, []string{sessionID})
	if err != nil {
		return Status{}, err
	}
	if !visibleIDs[sessionID] {
		return Status{}, gorm.ErrRecordNotFound
	}

	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, actorKey := actorKeyFromContext(ctx)
	now := time.Now()
	row := ReadState{
		TenantID:  tenantID,
		UserID:    userID,
		ActorKey:  actorKey,
		SessionID: sessionID,
		ReadAt:    now,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "tenant_id"},
			{Name: "actor_key"},
			{Name: "session_id"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"user_id":    userID,
			"read_at":    now,
			"updated_at": now,
		}),
	}).Create(&row).Error; err != nil {
		return Status{}, err
	}

	statuses, err := s.ListStatus(ctx, []string{sessionID})
	if err != nil {
		return Status{}, err
	}
	status := statuses[sessionID]
	status.Unread = false
	return status, nil
}

type visibleSessionRow struct {
	ID string
}

func (s *Service) visibleSessionIDs(ctx context.Context, sessionIDs []string) (map[string]bool, error) {
	out := make(map[string]bool)
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return out, nil
	}
	ownerID := types.SessionOwnerIDFromContext(ctx)
	query := s.db.WithContext(ctx).
		Table("sessions").
		Select("id").
		Where("tenant_id = ? AND deleted_at IS NULL AND id IN ?", tenantID, sessionIDs)
	if ownerID != "" {
		query = query.Where("(user_id = ? OR user_id IS NULL OR user_id = '')", ownerID)
	}
	var rows []visibleSessionRow
	if err := query.Find(&rows).Error; err != nil {
		return out, err
	}
	for _, row := range rows {
		if row.ID != "" {
			out[row.ID] = true
		}
	}
	return out, nil
}

type assistantMessageRow struct {
	ID          string
	SessionID   string
	IsCompleted bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (r assistantMessageRow) lastAt() time.Time {
	if r.UpdatedAt.After(r.CreatedAt) {
		return r.UpdatedAt
	}
	return r.CreatedAt
}

func (s *Service) lastAssistantMessages(ctx context.Context, sessionIDs []string) (map[string]assistantMessageRow, error) {
	out := make(map[string]assistantMessageRow)
	if len(sessionIDs) == 0 {
		return out, nil
	}
	var rows []assistantMessageRow
	if err := s.db.WithContext(ctx).
		Table("messages").
		Select("id, session_id, is_completed, created_at, updated_at").
		Where("session_id IN ? AND role = ? AND deleted_at IS NULL", sessionIDs, "assistant").
		Order("session_id ASC, created_at DESC, updated_at DESC").
		Find(&rows).Error; err != nil {
		return out, err
	}
	for _, row := range rows {
		if row.SessionID == "" {
			continue
		}
		if _, exists := out[row.SessionID]; !exists {
			out[row.SessionID] = row
		}
	}
	return out, nil
}

func (s *Service) readStates(ctx context.Context, tenantID uint64, actorKey string, sessionIDs []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time)
	if tenantID == 0 || actorKey == "" || len(sessionIDs) == 0 {
		return out, nil
	}
	var rows []ReadState
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND actor_key = ? AND session_id IN ?", tenantID, actorKey, sessionIDs).
		Find(&rows).Error; err != nil {
		return out, err
	}
	for _, row := range rows {
		out[row.SessionID] = row.ReadAt
	}
	return out, nil
}

func actorKeyFromContext(ctx context.Context) (string, string) {
	userID, _ := types.UserIDFromContext(ctx)
	if principal, ok := types.PrincipalFromContext(ctx); ok {
		if storageID := principal.StorageID(); storageID != "" {
			return userID, "principal:" + storageID
		}
	}
	if userID != "" && !types.IsSyntheticUserID(userID) {
		return userID, "user:" + userID
	}
	if userID != "" {
		return userID, "synthetic:" + userID
	}
	return "", "anonymous"
}

func normalizeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= 120 {
			break
		}
	}
	return out
}

func mapKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	return out
}
