package im

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newIMDuplicateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + strings.NewReplacer("/", "_", "\\", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	statements := []string{
		`CREATE TABLE custom_agents (
			id varchar(36) NOT NULL,
			tenant_id integer NOT NULL,
			name varchar(255) NOT NULL,
			deleted_at datetime,
			PRIMARY KEY (id, tenant_id)
		)`,
		`CREATE TABLE im_channels (
			id varchar(36) PRIMARY KEY,
			tenant_id integer NOT NULL,
			agent_id varchar(36) NOT NULL,
			platform varchar(20) NOT NULL,
			name varchar(255) NOT NULL DEFAULT '',
			enabled numeric NOT NULL DEFAULT 1,
			mode varchar(20) NOT NULL DEFAULT 'websocket',
			output_mode varchar(20) NOT NULL DEFAULT 'stream',
			knowledge_base_id varchar(36) DEFAULT '',
			bot_identity varchar(255) NOT NULL DEFAULT '',
			session_mode varchar(20) NOT NULL DEFAULT 'user',
			credentials json NOT NULL DEFAULT '{}',
			created_at datetime,
			updated_at datetime,
			deleted_at datetime
		)`,
		`CREATE UNIQUE INDEX idx_im_channels_bot_identity
			ON im_channels (bot_identity)
			WHERE deleted_at IS NULL AND bot_identity != ''`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("migrate test database: %v", err)
		}
	}
	return db
}

func newIMDuplicateTestService(db *gorm.DB) *Service {
	return &Service{
		db:       db,
		channels: make(map[string]*channelState),
	}
}

func newIMDuplicateTestChannel(id, agentID, botID string) *IMChannel {
	return &IMChannel{
		ID:          id,
		TenantID:    42,
		AgentID:     agentID,
		Platform:    "wecom",
		Mode:        "websocket",
		OutputMode:  "stream",
		Enabled:     false,
		Credentials: types.JSON(fmt.Sprintf(`{"bot_id":%q}`, botID)),
	}
}

func TestCheckDuplicateBotReclaimsDeletedAgentBinding(t *testing.T) {
	db := newIMDuplicateTestDB(t)
	deletedAt := time.Now()
	if err := db.Exec(
		"INSERT INTO custom_agents (id, tenant_id, name, deleted_at) VALUES (?, ?, ?, ?)",
		"agent-deleted", 42, "deleted agent", deletedAt,
	).Error; err != nil {
		t.Fatalf("create deleted agent fixture: %v", err)
	}

	stale := newIMDuplicateTestChannel("stale-channel", "agent-deleted", "bot-1")
	if err := db.Create(stale).Error; err != nil {
		t.Fatalf("create stale channel fixture: %v", err)
	}

	service := newIMDuplicateTestService(db)
	candidate := newIMDuplicateTestChannel("new-channel", "agent-new", "bot-1")
	if err := service.checkDuplicateBot(candidate, ""); err != nil {
		t.Fatalf("checkDuplicateBot() error = %v, want stale binding reclaimed", err)
	}
	if err := db.Create(candidate).Error; err != nil {
		t.Fatalf("create replacement channel: %v", err)
	}

	var deleted IMChannel
	if err := db.First(&deleted, "id = ?", stale.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("stale channel default-scope lookup error = %v, want not found", err)
	}
	var stored IMChannel
	if err := db.Unscoped().First(&stored, "id = ?", stale.ID).Error; err != nil {
		t.Fatalf("load reclaimed channel: %v", err)
	}
	if !stored.DeletedAt.Valid {
		t.Fatal("stale channel was not soft-deleted")
	}
}

func TestCheckDuplicateBotRejectsActiveAgentBinding(t *testing.T) {
	db := newIMDuplicateTestDB(t)
	if err := db.Exec(
		"INSERT INTO custom_agents (id, tenant_id, name, deleted_at) VALUES (?, ?, ?, NULL)",
		"agent-active", 42, "active agent",
	).Error; err != nil {
		t.Fatalf("create active agent fixture: %v", err)
	}
	existing := newIMDuplicateTestChannel("existing-channel", "agent-active", "bot-1")
	if err := db.Create(existing).Error; err != nil {
		t.Fatalf("create existing channel fixture: %v", err)
	}

	service := newIMDuplicateTestService(db)
	candidate := newIMDuplicateTestChannel("new-channel", "agent-new", "bot-1")
	err := service.checkDuplicateBot(candidate, "")
	if err == nil || !strings.HasPrefix(err.Error(), "duplicate_bot:") {
		t.Fatalf("checkDuplicateBot() error = %v, want duplicate_bot", err)
	}
}

func TestCheckDuplicateBotRejectsBuiltinAgentBinding(t *testing.T) {
	db := newIMDuplicateTestDB(t)
	existing := newIMDuplicateTestChannel("existing-channel", types.BuiltinKnowledgeQAID, "bot-1")
	if err := db.Create(existing).Error; err != nil {
		t.Fatalf("create built-in channel fixture: %v", err)
	}

	service := newIMDuplicateTestService(db)
	candidate := newIMDuplicateTestChannel("new-channel", "agent-new", "bot-1")
	err := service.checkDuplicateBot(candidate, "")
	if err == nil || !strings.HasPrefix(err.Error(), "duplicate_bot:") {
		t.Fatalf("checkDuplicateBot() error = %v, want duplicate_bot", err)
	}
}
