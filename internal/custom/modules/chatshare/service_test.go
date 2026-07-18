package chatshare

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type chatShareSessionServiceStub struct {
	interfaces.SessionService
	session *types.Session
}

// chatShareMessageRow mirrors only the message columns used by this module.
// The production Message model contains legacy PostgreSQL-specific GORM tags
// that SQLite cannot AutoMigrate, so tests use this portable table fixture.
type chatShareMessageRow struct {
	ID                  string `gorm:"primaryKey;type:varchar(36)"`
	SessionID           string `gorm:"index"`
	RequestID           string
	Content             string
	Role                string
	KnowledgeReferences types.References         `gorm:"type:text"`
	AgentSteps          types.AgentSteps         `gorm:"type:text"`
	MentionedItems      types.MentionedItems     `gorm:"type:text"`
	Images              types.MessageImages      `gorm:"type:text"`
	Attachments         types.MessageAttachments `gorm:"type:text"`
	IsCompleted         bool
	IsFallback          bool
	Channel             string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	DeletedAt           gorm.DeletedAt `gorm:"index"`
}

func (chatShareMessageRow) TableName() string { return "messages" }

func (s *chatShareSessionServiceStub) GetSession(_ context.Context, id string) (*types.Session, error) {
	if s.session == nil || s.session.ID != id {
		return nil, gorm.ErrRecordNotFound
	}
	return s.session, nil
}

func chatShareTestContext() context.Context {
	ctx := types.WithPrincipal(context.Background(), types.Principal{
		Type: types.PrincipalWebUser,
		ID:   "user-1",
	})
	ctx = context.WithValue(ctx, types.TenantIDContextKey, uint64(7))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "user-1")
	return ctx
}

func newChatShareTestService(t *testing.T) (*Service, *gorm.DB, context.Context) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&chatShareMessageRow{}, &artifactRow{}))
	svc := NewService(
		db,
		&chatShareSessionServiceStub{session: &types.Session{
			ID:       "session-1",
			Title:    "选择性分享测试",
			TenantID: 7,
			UserID:   "user-1",
		}},
		nil,
		nil,
		"https://example.test",
	)
	require.NoError(t, svc.Migrate(context.Background()))
	return svc, db, chatShareTestContext()
}

func insertChatShareMessage(t *testing.T, db *gorm.DB, message types.Message) {
	t.Helper()
	require.NoError(t, db.Create(&chatShareMessageRow{
		ID:                  message.ID,
		SessionID:           message.SessionID,
		RequestID:           message.RequestID,
		Content:             message.Content,
		Role:                message.Role,
		KnowledgeReferences: message.KnowledgeReferences,
		AgentSteps:          message.AgentSteps,
		MentionedItems:      message.MentionedItems,
		Images:              message.Images,
		Attachments:         message.Attachments,
		IsCompleted:         message.IsCompleted,
		IsFallback:          message.IsFallback,
		Channel:             message.Channel,
		CreatedAt:           message.CreatedAt,
		UpdatedAt:           message.UpdatedAt,
		DeletedAt:           message.DeletedAt,
	}).Error)
}

func TestCreateSharePersistsOnlySelectedMessagesAndResources(t *testing.T) {
	svc, db, ctx := newChatShareTestService(t)
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	insertChatShareMessage(t, db, types.Message{
		ID:          "q1",
		SessionID:   "session-1",
		RequestID:   "r1",
		Role:        "user",
		Content:     "问题一",
		IsCompleted: true,
		Images:      types.MessageImages{{URL: "local://7/questions/q1.png"}},
		CreatedAt:   base,
		UpdatedAt:   base,
	})
	insertChatShareMessage(t, db, types.Message{
		ID:          "a1",
		SessionID:   "session-1",
		RequestID:   "r1",
		Role:        "assistant",
		Content:     "回答一 ![图](local://7/answers/a1.png)",
		IsCompleted: true,
		CreatedAt:   base.Add(time.Second),
		UpdatedAt:   base.Add(time.Second),
	})
	insertChatShareMessage(t, db, types.Message{
		ID:          "q2",
		SessionID:   "session-1",
		RequestID:   "r2",
		Role:        "user",
		Content:     "问题二",
		IsCompleted: true,
		Images:      types.MessageImages{{URL: "local://7/questions/q2-secret.png"}},
		CreatedAt:   base.Add(2 * time.Second),
		UpdatedAt:   base.Add(2 * time.Second),
	})
	insertChatShareMessage(t, db, types.Message{
		ID:          "a2",
		SessionID:   "session-1",
		RequestID:   "r2",
		Role:        "assistant",
		Content:     "仍在生成",
		IsCompleted: false,
		CreatedAt:   base.Add(3 * time.Second),
		UpdatedAt:   base.Add(3 * time.Second),
	})
	require.NoError(t, db.Create(&artifactRow{
		ID:        "artifact-a1",
		TenantID:  7,
		SessionID: "session-1",
		MessageID: "a1",
		FilePath:  "/tmp/a1.csv",
		FileName:  "a1.csv",
		CreatedAt: base.Add(time.Second),
	}).Error)
	require.NoError(t, db.Create(&artifactRow{
		ID:        "artifact-q2",
		TenantID:  7,
		SessionID: "session-1",
		MessageID: "q2",
		FilePath:  "/tmp/q2.csv",
		FileName:  "q2.csv",
		CreatedAt: base.Add(2 * time.Second),
	}).Error)

	candidates, err := svc.GetCandidates(ctx, "session-1")
	require.NoError(t, err)
	require.Len(t, candidates.Messages, 4)
	require.True(t, candidates.Messages[0].Selectable)
	require.True(t, candidates.Messages[1].Selectable)
	require.Equal(t, candidates.Messages[0].TurnID, candidates.Messages[1].TurnID)
	require.False(t, candidates.Messages[2].Selectable)
	require.False(t, candidates.Messages[3].Selectable)
	require.Equal(t, candidates.Messages[2].TurnID, candidates.Messages[3].TurnID)
	require.NotEmpty(t, candidates.Messages[3].DisabledReason)

	link, err := svc.CreateShare(ctx, "session-1", []string{"a1", "q1", "q1"})
	require.NoError(t, err)
	require.NotEmpty(t, link.Token)

	var snapshots []MessageSnapshot
	require.NoError(t, db.Where("share_id = ?", link.ID).Order("seq ASC").Find(&snapshots).Error)
	require.Len(t, snapshots, 2)
	require.Equal(t, "q1", snapshots[0].OriginalMessageID)
	require.Equal(t, "a1", snapshots[1].OriginalMessageID)

	var resources []ResourceSnapshot
	require.NoError(t, db.Where("share_id = ?", link.ID).Find(&resources).Error)
	resourceKeys := make(map[string]bool, len(resources))
	for _, resource := range resources {
		resourceKeys[resource.ResourceType+":"+resource.ResourceKey] = true
	}
	require.True(t, resourceKeys[ResourceTypeFile+":local://7/questions/q1.png"])
	require.True(t, resourceKeys[ResourceTypeFile+":local://7/answers/a1.png"])
	require.True(t, resourceKeys[ResourceTypeArtifact+":artifact-a1"])
	require.False(t, resourceKeys[ResourceTypeFile+":local://7/questions/q2-secret.png"])
	require.False(t, resourceKeys[ResourceTypeArtifact+":artifact-q2"])
	require.NoError(t, svc.resourceAllowed(ctx, link.ID, ResourceTypeArtifact, "artifact-a1"))
	require.ErrorIs(t, svc.resourceAllowed(ctx, link.ID, ResourceTypeArtifact, "artifact-q2"), gorm.ErrRecordNotFound)

	_, err = svc.CreateShare(ctx, "session-1", []string{"q1"})
	require.ErrorIs(t, err, ErrInvalidMessageSelection)
	_, err = svc.CreateShare(ctx, "session-1", []string{"a1"})
	require.ErrorIs(t, err, ErrInvalidMessageSelection)
}

func TestCreateShareRejectsInvalidOrIncompleteSelection(t *testing.T) {
	svc, db, ctx := newChatShareTestService(t)
	now := time.Now().UTC()
	insertChatShareMessage(t, db, types.Message{
		ID:          "pending-answer",
		SessionID:   "session-1",
		RequestID:   "r1",
		Role:        "assistant",
		Content:     "生成中",
		IsCompleted: false,
		CreatedAt:   now,
		UpdatedAt:   now,
	})

	_, err := svc.CreateShare(ctx, "session-1", nil)
	require.ErrorIs(t, err, ErrInvalidMessageSelection)
	_, err = svc.CreateShare(ctx, "session-1", []string{"missing"})
	require.ErrorIs(t, err, ErrInvalidMessageSelection)
	_, err = svc.CreateShare(ctx, "session-1", []string{"pending-answer"})
	require.ErrorIs(t, err, ErrInvalidMessageSelection)
}

func TestProviderResourcesFromContent(t *testing.T) {
	resources := providerResourcesFromContent(
		`![一](local:&#x2f;&#x2f;7/a.png) ![二](s3://bucket/7/b.png), https://example.test/x.png`,
	)
	require.Equal(t, []string{"local://7/a.png", "s3://bucket/7/b.png"}, resources)
}

func TestBuildShareTurnsNeverPairsAdjacentMessagesWithoutRequestID(t *testing.T) {
	turns := buildShareTurns([]types.Message{
		{ID: "q1", Role: "user", IsCompleted: true},
		{ID: "a1", Role: "assistant", IsCompleted: true},
	})

	require.Len(t, turns, 2)
	require.False(t, turns[0].Selectable)
	require.False(t, turns[1].Selectable)
	require.NotEqual(t, turns[0].ID, turns[1].ID)
}

func TestBuildShareTurnsUsesExactRequestIDAndRejectsEmptyAnswer(t *testing.T) {
	turns := buildShareTurns([]types.Message{
		{ID: "q-missing", RequestID: "r-missing", Role: "user", IsCompleted: true},
		{ID: "q-valid", RequestID: "r-valid", Role: "user", IsCompleted: true},
		{ID: "a-valid", RequestID: "r-valid", Role: "assistant", Content: "正确回答", IsCompleted: true},
		{ID: "a-other", RequestID: "r-other", Role: "assistant", Content: "不能串给前一个问题", IsCompleted: true},
		{ID: "q-empty", RequestID: "r-empty", Role: "user", IsCompleted: true},
		{ID: "a-empty", RequestID: "r-empty", Role: "assistant", Content: "  ", IsCompleted: true},
	})

	require.Len(t, turns, 4)
	selectableIDs := make([]string, 0)
	for _, turn := range turns {
		if turn.Selectable {
			selectableIDs = append(selectableIDs, turn.ID)
		}
	}
	require.Equal(t, []string{"request:r-valid"}, selectableIDs)
	require.Contains(t, turns[3].DisabledReason, "无有效内容")
}
