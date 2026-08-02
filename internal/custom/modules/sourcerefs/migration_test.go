package sourcerefs

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMigrateStoredAgentCitationPromptsUsesOnlyCurrentProtocol(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:source-prompt-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&types.CustomAgent{}); err != nil {
		t.Fatal(err)
	}
	agent := &types.CustomAgent{
		ID: "agent-1", TenantID: 1, Name: "agent", Config: types.CustomAgentConfig{
			SystemPrompt: "Keep this role.\n*   **Sourced (Inline Citations):** claim.<kb doc=\"A\" chunk_id=\"x\" />\n*   **Structured:** Keep structure.",
		},
	}
	if err := db.Create(agent).Error; err != nil {
		t.Fatal(err)
	}
	deleted := &types.CustomAgent{
		ID: "agent-deleted", TenantID: 1, Name: "deleted", Config: types.CustomAgentConfig{
			SystemPrompt: "Deleted role.\n*   **Sourced (Inline Citations):** claim.<web url=\"https://example.com\" />",
		},
	}
	if err := db.Create(deleted).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(deleted).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateStoredAgentCitationPrompts(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var got types.CustomAgent
	if err := db.First(&got, "id = ? AND tenant_id = ?", agent.ID, agent.TenantID).Error; err != nil {
		t.Fatal(err)
	}
	prompt := got.Config.SystemPrompt
	if strings.Contains(prompt, "<kb ") || strings.Contains(prompt, "<web ") {
		t.Fatalf("retired protocol remained in stored prompt: %s", prompt)
	}
	for _, expected := range []string{"Keep this role.", "**Structured:** Keep structure.", generationContractMarker, `<src id="S1" />`} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("stored prompt missing %q: %s", expected, prompt)
		}
	}
	if err := migrateStoredAgentCitationPrompts(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var twice types.CustomAgent
	if err := db.First(&twice, "id = ? AND tenant_id = ?", agent.ID, agent.TenantID).Error; err != nil {
		t.Fatal(err)
	}
	if twice.Config.SystemPrompt != prompt {
		t.Fatalf("prompt migration is not idempotent")
	}
	var migratedDeleted types.CustomAgent
	if err := db.Unscoped().First(&migratedDeleted, "id = ? AND tenant_id = ?", deleted.ID, deleted.TenantID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(migratedDeleted.Config.SystemPrompt, "<web ") ||
		!strings.Contains(migratedDeleted.Config.SystemPrompt, generationContractMarker) {
		t.Fatalf("soft-deleted prompt was not normalized: %s", migratedDeleted.Config.SystemPrompt)
	}
}
