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
