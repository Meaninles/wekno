package skillhub

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newLightweightRuntimeTestService(t *testing.T) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	svc := NewService(db)
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate skillhub: %v", err)
	}
	if err := db.AutoMigrate(&types.User{}, &types.Organization{}, &types.OrganizationTenantMember{}); err != nil {
		t.Fatalf("migrate related tables: %v", err)
	}
	return svc
}

func lightweightRuntimeTestContext() context.Context {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(10001))
	return context.WithValue(ctx, types.UserIDContextKey, "lightweight-runtime-test-user")
}

func writePreloadedLightweightSkill(t *testing.T, root, dir, name, instructions string) {
	t.Helper()
	skillDir := filepath.Join(root, dir)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("create skill dir: %v", err)
	}
	content := "---\nname: " + name + "\ndescription: test description\n---\n\n" + instructions + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

func TestLightweightPackagesConfiguredAndChatSelectionResolveIdentically(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WEKNORA_SKILLS_DIR", root)
	writePreloadedLightweightSkill(t, root, "policy-helper", "制度助手", "严格按照制度流程回答。")
	svc := newLightweightRuntimeTestService(t)

	configured, configuredDrops, err := svc.LightweightPackages(
		lightweightRuntimeTestContext(), "selected", []string{"制度助手"}, nil,
	)
	if err != nil {
		t.Fatalf("resolve configured skill: %v", err)
	}
	chat, chatDrops, err := svc.LightweightPackages(
		lightweightRuntimeTestContext(), "none", nil, []string{"制度助手"},
	)
	if err != nil {
		t.Fatalf("resolve chat skill: %v", err)
	}
	if len(configuredDrops) != 0 || len(chatDrops) != 0 {
		t.Fatalf("unexpected drops: configured=%v chat=%v", configuredDrops, chatDrops)
	}
	if !reflect.DeepEqual(configured, chat) {
		t.Fatalf("configured and chat resolution differ:\nconfigured=%+v\nchat=%+v", configured, chat)
	}
}

func TestLightweightPackagesAllModeDoesNotDuplicateChatSelection(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WEKNORA_SKILLS_DIR", root)
	writePreloadedLightweightSkill(t, root, "policy-helper", "制度助手", "严格按照制度流程回答。")
	svc := newLightweightRuntimeTestService(t)

	packages, dropped, err := svc.LightweightPackages(
		lightweightRuntimeTestContext(), "all", nil, []string{"制度助手", "制度助手"},
	)
	if err != nil {
		t.Fatalf("resolve all skills: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("unexpected drops: %v", dropped)
	}
	if len(packages) != 1 || packages[0].Name != "制度助手" {
		t.Fatalf("packages = %+v, want one deduplicated 制度助手", packages)
	}
}

func TestLightweightPackagesReportsUnavailableNames(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WEKNORA_SKILLS_DIR", root)
	svc := newLightweightRuntimeTestService(t)

	packages, dropped, err := svc.LightweightPackages(
		lightweightRuntimeTestContext(), "selected", []string{"已删除技能"}, nil,
	)
	if err != nil {
		t.Fatalf("resolve unavailable skill: %v", err)
	}
	if len(packages) != 0 || len(dropped) != 1 || dropped[0].Reason != LightweightSkillDropUnavailable {
		t.Fatalf("packages=%+v dropped=%+v, want one unavailable drop", packages, dropped)
	}
}

func TestLightweightPackagesNoSelectionNeedsNoTenantLookup(t *testing.T) {
	svc := newLightweightRuntimeTestService(t)
	packages, dropped, err := svc.LightweightPackages(context.Background(), "none", nil, nil)
	if err != nil {
		t.Fatalf("empty selection should not require a tenant lookup: %v", err)
	}
	if len(packages) != 0 || len(dropped) != 0 {
		t.Fatalf("packages=%+v dropped=%+v, want empty resolution", packages, dropped)
	}
}
