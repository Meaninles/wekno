package runtimeprofile

import "testing"

func TestValidateClusterBudgetAcceptsBaselineLayout(t *testing.T) {
	got, err := ValidateClusterBudget(100, []ReplicaBudget{
		{Role: RoleAPI, Replicas: 3, MaxOpen: 6},
		{Role: RoleParseWorker, Replicas: 2, MaxOpen: 6},
		{Role: RoleDerivativeWorker, Replicas: 2, MaxOpen: 4},
		{Role: RoleWikiWorker, Replicas: 2, MaxOpen: 3},
		{Role: RoleMaintenance, Replicas: 2, MaxOpen: 2},
		{Role: RoleMigration, Replicas: 1, MaxOpen: 1},
	})
	if err != nil {
		t.Fatalf("ValidateClusterBudget() error = %v", err)
	}
	if got.Steady != 49 {
		t.Fatalf("steady = %d, want 49", got.Steady)
	}
}

func TestValidateClusterBudgetRejectsSteadyOverSixtyPercent(t *testing.T) {
	_, err := ValidateClusterBudget(100, []ReplicaBudget{
		{Role: RoleAPI, Replicas: 11, MaxOpen: 6},
	})
	if err == nil {
		t.Fatal("ValidateClusterBudget() accepted an unsafe steady budget")
	}
}

func TestValidateClusterBudgetRejectsRollingOverSeventyPercent(t *testing.T) {
	_, err := ValidateClusterBudget(100, []ReplicaBudget{
		{Role: RoleAPI, Replicas: 10, MaxOpen: 6, RollingMax: 2},
	})
	if err == nil {
		t.Fatal("ValidateClusterBudget() accepted an unsafe rolling budget")
	}
}

func TestValidateClusterBudgetFromEnvHonorsZeroSurgeRollout(t *testing.T) {
	t.Setenv("CUSTOM_DATABASE_POOL_BUDGET_ENFORCED", "true")
	t.Setenv("CUSTOM_POSTGRES_MAX_CONNECTIONS", "100")
	for _, key := range []string{
		"CUSTOM_RUNTIME_API_ROLLING_MAX",
		"CUSTOM_RUNTIME_PARSE_WORKER_ROLLING_MAX",
		"CUSTOM_RUNTIME_DERIVATIVE_WORKER_ROLLING_MAX",
		"CUSTOM_RUNTIME_WIKI_WORKER_ROLLING_MAX",
		"CUSTOM_RUNTIME_MAINTENANCE_ROLLING_MAX",
		"CUSTOM_RUNTIME_MIGRATION_ROLLING_MAX",
	} {
		t.Setenv(key, "0")
	}
	t.Setenv("CUSTOM_RUNTIME_API_REPLICAS", "3")
	t.Setenv("CUSTOM_RUNTIME_API_MAX_OPEN_CONNS", "6")
	t.Setenv("CUSTOM_RUNTIME_PARSE_WORKER_REPLICAS", "3")
	t.Setenv("CUSTOM_RUNTIME_PARSE_WORKER_MAX_OPEN_CONNS", "5")
	t.Setenv("CUSTOM_RUNTIME_DERIVATIVE_WORKER_REPLICAS", "2")
	t.Setenv("CUSTOM_RUNTIME_DERIVATIVE_WORKER_MAX_OPEN_CONNS", "4")
	t.Setenv("CUSTOM_RUNTIME_WIKI_WORKER_REPLICAS", "2")
	t.Setenv("CUSTOM_RUNTIME_WIKI_WORKER_MAX_OPEN_CONNS", "4")
	t.Setenv("CUSTOM_RUNTIME_MAINTENANCE_REPLICAS", "2")
	t.Setenv("CUSTOM_RUNTIME_MAINTENANCE_MAX_OPEN_CONNS", "3")
	t.Setenv("CUSTOM_RUNTIME_MIGRATION_REPLICAS", "1")
	t.Setenv("CUSTOM_RUNTIME_MIGRATION_MAX_OPEN_CONNS", "3")

	got, err := ValidateClusterBudgetFromEnv()
	if err != nil {
		t.Fatalf("ValidateClusterBudgetFromEnv() error = %v", err)
	}
	if got.Steady != 58 || got.Rolling != 58 {
		t.Fatalf("budget = steady:%d rolling:%d, want 58/58", got.Steady, got.Rolling)
	}
}
