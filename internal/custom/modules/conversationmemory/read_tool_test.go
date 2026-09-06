package conversationmemory

import (
	"context"
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"strings"
	"testing"
)

func TestReadConversationScopePaginationAndCancellation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := db.DB()
	conn.SetMaxOpenConns(1)
	defer conn.Close()
	for _, q := range []string{`CREATE TABLE sessions (id TEXT, tenant_id INTEGER, user_id TEXT, deleted_at DATETIME)`, `CREATE TABLE messages (id TEXT,session_id TEXT,role TEXT,content TEXT,created_at DATETIME,deleted_at DATETIME)`, `INSERT INTO sessions VALUES ('own',7,'user',NULL),('foreign',8,'other',NULL),('deleted',7,'user',CURRENT_TIMESTAMP)`} {
		if err = db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	text := strings.Repeat("正文", 9000) + "final fact"
	for _, row := range [][]any{{"long", "own", "user", text}, {"assistant", "own", "assistant", "untrusted"}, {"foreign-msg", "foreign", "user", "secret"}} {
		if err = db.Exec("INSERT INTO messages(id,session_id,role,content) VALUES (?,?,?,?)", row...).Error; err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.WithValue(context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7)), types.UserIDContextKey, "user")
	tool := &ReadTool{DB: db, SessionID: "own"}
	r, err := tool.Execute(ctx, json.RawMessage(`{"source_id":"user_message_long"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Output, "next_offset") || strings.Contains(r.Output, "final fact") {
		t.Fatal(r.Output)
	}
	r, err = tool.Execute(ctx, json.RawMessage(`{"source_id":"user_message_long","offset":16000}`))
	if err != nil || !strings.Contains(r.Output, "final fact") {
		t.Fatal(r, err)
	}
	for _, id := range []string{"foreign-msg", "assistant"} {
		r, err = tool.Execute(ctx, json.RawMessage(`{"source_id":"user_message_`+id+`"}`))
		if err != nil || strings.Contains(r.Output, "secret") || strings.Contains(r.Output, "untrusted") {
			t.Fatal(r, err)
		}
	}
	r, err = tool.Execute(ctx, json.RawMessage(`{"source_id":"assistant_message_assistant"}`))
	if err != nil || !strings.Contains(r.Output, `"role":"assistant"`) || !strings.Contains(r.Output, "untrusted") {
		t.Fatal("assistant source cannot be read", r, err)
	}
	for _, id := range []string{"foreign", "deleted"} {
		tool.SessionID = id
		if _, err = tool.Execute(ctx, json.RawMessage(`{}`)); err == nil {
			t.Fatal("foreign/deleted session accessible", id)
		}
	}
	tool.SessionID = "own"
	wrongUser := context.WithValue(ctx, types.UserIDContextKey, "other")
	if _, err = tool.Execute(wrongUser, json.RawMessage(`{}`)); err == nil {
		t.Fatal("foreign user accessible")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = tool.Execute(cancelled, json.RawMessage(`{}`)); err == nil {
		t.Fatal("cancel ignored")
	}
	if _, err = tool.Execute(ctx, json.RawMessage(`{"offset":-1}`)); err == nil {
		t.Fatal("negative offset accepted")
	}
	archived := WithLiveTools(ctx)
	LiveToolsFromContext(archived).outputs["current-call"] = text
	r, err = tool.Execute(archived, json.RawMessage(`{"tool_call_id":"current-call","section":"tools","offset":16000}`))
	if err != nil || !strings.Contains(r.Output, "final fact") {
		t.Fatal("current run archive could not be read", r, err)
	}
	for _, inaccessible := range []context.Context{ctx, WithLiveTools(ctx), context.WithValue(archived, types.UserIDContextKey, "other")} {
		if _, err = tool.Execute(inaccessible, json.RawMessage(`{"tool_call_id":"current-call","section":"tools"}`)); err == nil {
			t.Fatal("archive escaped its session, user or run")
		}
	}
}
