package chatuploads

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type sessionStore struct {
	interfaces.SessionService
	db *gorm.DB
}

func (s sessionStore) GetSession(ctx context.Context, id string) (*types.Session, error) {
	var row types.Session
	err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", id, types.MustTenantIDFromContext(ctx)).Take(&row).Error
	return &row, err
}
func actor(tenant uint64, user string) context.Context {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, tenant)
	return context.WithValue(ctx, types.UserIDContextKey, user)
}

func TestRepairPostgresUploadScopeHistoryAndAcceptance(t *testing.T) {
	db := testsupport.Postgres(t, &types.KnowledgeBase{}, &types.Knowledge{}, &types.Session{}, &types.Message{})
	s := NewService(db, sessionStore{db: db}, nil, nil, nil)
	require.NoError(t, s.Migrate(context.Background()))
	require.NoError(t, s.Migrate(context.Background()))
	sid, otherSID := uuid.NewString(), uuid.NewString()
	sessions := []*types.Session{{ID: sid, TenantID: 7, UserID: "owner"}, {ID: otherSID, TenantID: 7, UserID: "owner"}}
	for _, row := range sessions {
		require.NoError(t, db.Create(row).Error)
	}
	sid, otherSID = sessions[0].ID, sessions[1].ID
	kb := &types.KnowledgeBase{ID: privateKBID(sid), TenantID: 7, Name: "private", IsTemporary: true, ChatSessionID: sid, ChatOwnerID: "owner"}
	require.NoError(t, db.Create(kb).Error)
	require.True(t, kb.AllowsPrivateAccess(actor(7, "owner")))
	require.False(t, kb.AllowsPrivateAccess(actor(7, "tenant-admin")))
	require.False(t, kb.AllowsPrivateAccess(actor(8, "owner")))
	sentID, unsentID, otherID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for i, id := range []string{sentID, unsentID, otherID} {
		k := &types.Knowledge{ID: id, TenantID: 7, KnowledgeBaseID: kb.ID, Type: "file", FileName: fmt.Sprintf("file%d.xlsx", i), EnableStatus: "enabled", PublicationState: "published", CoreStatus: types.CoreStatusReady, ParseStatus: types.ParseStatusCompleted, PublishedGeneration: "generation", ProcessingGeneration: "generation"}
		if i == 2 {
			k.KnowledgeBaseID = privateKBID(otherSID)
		}
		require.NoError(t, db.Create(k).Error)
	}
	// Cross-session and unsent sources are never made available by history.
	msg := &types.Message{ID: uuid.NewString(), SessionID: sid, Role: "user", Content: "inspect attached source", Attachments: types.MessageAttachments{{KnowledgeID: sentID}, {KnowledgeID: otherID}}}
	require.NoError(t, db.Create(msg).Error)
	ids, err := s.HistoryTargets(actor(7, "owner"), sid)
	require.NoError(t, err)
	require.Equal(t, []string{sentID}, ids)
	_, err = s.HistoryTargets(actor(7, "tenant-admin"), sid)
	require.Error(t, err)
	_, _, err = s.Resolve(actor(7, "owner"), otherSID, []string{sentID})
	require.Error(t, err)
	_, _, err = s.Resolve(actor(7, "owner"), sid, []string{sentID})
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", sentID).Updates(map[string]any{"processing_generation": "draft", "core_status": types.CoreStatusProcessing}).Error)
	_, _, err = s.Resolve(actor(7, "owner"), sid, []string{sentID})
	require.Error(t, err, "a new upload send must await exact generation readiness")
	ids, err = s.HistoryTargets(actor(7, "owner"), sid)
	require.NoError(t, err)
	require.Equal(t, []string{sentID}, ids, "existing published evidence survives a reparse")
	require.NoError(t, db.Delete(&types.Knowledge{ID: sentID}).Error)
	ids, err = s.HistoryTargets(actor(7, "owner"), sid)
	require.NoError(t, err)
	require.Empty(t, ids)
	encoded, err := json.Marshal(kb)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "chat_owner_id")

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.withAcceptance(actor(7, "owner"), sessions[0], func() error { close(held); <-release; return nil })
	}()
	<-held
	start := time.Now()
	err = s.withAcceptance(actor(7, "owner"), sessions[0], func() error { t.Error("must not enter held acceptance"); return nil })
	require.ErrorIs(t, err, ErrBusy)
	require.Less(t, time.Since(start), time.Second)
	close(release)
	require.NoError(t, <-done)
	cancelled, cancel := context.WithCancel(actor(7, "owner"))
	require.NoError(t, s.withAcceptance(cancelled, sessions[0], func() error { cancel(); return nil }))
	require.NoError(t, s.withAcceptance(actor(7, "owner"), sessions[0], func() error { return nil }), "cancellation must release the session database lock")
}
