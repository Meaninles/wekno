package runtimeprofile

import "testing"

func TestLoadFromEnvDefaultsToDevAllOnlyWhenNotEnforced(t *testing.T) {
	clearRoleEnv(t)
	got, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if got.Role != RoleDevAll {
		t.Fatalf("role = %q, want %q", got.Role, RoleDevAll)
	}
}

func TestLoadFromEnvRequiresExplicitRoleWhenEnforced(t *testing.T) {
	clearRoleEnv(t)
	t.Setenv(enforcedEnv, "true")
	if _, err := LoadFromEnv(); err == nil {
		t.Fatal("LoadFromEnv() accepted a missing enforced role")
	}
}

func TestProductionRejectsDevAll(t *testing.T) {
	clearRoleEnv(t)
	t.Setenv(environmentEnv, "production")
	t.Setenv(roleEnv, string(RoleDevAll))
	if _, err := LoadFromEnv(); err == nil {
		t.Fatal("LoadFromEnv() accepted dev-all in production")
	}
}

func TestCapabilityMatrix(t *testing.T) {
	tests := []struct {
		role                                Role
		api, parse, derivative, wiki, maint bool
	}{
		{RoleAPI, true, false, false, false, false},
		{RoleParseWorker, false, true, false, false, false},
		{RoleDerivativeWorker, false, false, true, false, false},
		{RoleWikiWorker, false, false, false, true, false},
		{RoleMaintenance, false, false, false, false, true},
		{RoleMigration, false, false, false, false, false},
		{RoleDevAll, true, true, true, true, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			p := Profile{Role: tt.role}
			if p.ServesAPI() != tt.api || p.RunsParseWorker() != tt.parse ||
				p.RunsDerivativeWorker() != tt.derivative ||
				p.RunsWikiWorker() != tt.wiki || p.RunsMaintenance() != tt.maint {
				t.Fatalf("unexpected capability matrix for %s", tt.role)
			}
		})
	}
}

func clearRoleEnv(t *testing.T) {
	t.Helper()
	t.Setenv(roleEnv, "")
	t.Setenv(enforcedEnv, "")
	t.Setenv(environmentEnv, "")
	t.Setenv("WEKNORA_ENVIRONMENT", "")
	t.Setenv("APP_ENV", "")
}
