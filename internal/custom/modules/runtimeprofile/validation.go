package runtimeprofile

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type ReplicaBudget struct {
	Role       Role
	Replicas   int
	MaxOpen    int
	RollingMax int
}

type ClusterBudget struct {
	MaxConnections int
	Steady         int
	Rolling        int
	SteadyLimit    int
	RollingLimit   int
}

func ValidateClusterBudget(maxConnections int, rows []ReplicaBudget) (ClusterBudget, error) {
	if maxConnections <= 0 {
		return ClusterBudget{}, fmt.Errorf("PostgreSQL max_connections must be positive")
	}
	result := ClusterBudget{
		MaxConnections: maxConnections,
		SteadyLimit:    maxConnections * 60 / 100,
		RollingLimit:   maxConnections * 70 / 100,
	}
	for _, row := range rows {
		if row.Replicas < 0 || row.MaxOpen <= 0 || row.RollingMax < 0 {
			return ClusterBudget{}, fmt.Errorf("invalid connection budget for role %s", row.Role)
		}
		result.Steady += row.Replicas * row.MaxOpen
		rollingReplicas := row.Replicas + row.RollingMax
		result.Rolling += rollingReplicas * row.MaxOpen
	}
	if result.Steady > result.SteadyLimit {
		return result, fmt.Errorf(
			"steady application connection budget %d exceeds 60%% of PostgreSQL max_connections (%d)",
			result.Steady, result.SteadyLimit,
		)
	}
	if result.Rolling > result.RollingLimit {
		return result, fmt.Errorf(
			"rolling application connection budget %d exceeds 70%% of PostgreSQL max_connections (%d)",
			result.Rolling, result.RollingLimit,
		)
	}
	return result, nil
}

// ValidateClusterBudgetFromEnv is a startup preflight shared by every role.
// It is intentionally opt-in outside production because a developer may run
// only one slice of the topology. Production defaults to fail-closed.
func ValidateClusterBudgetFromEnv() (ClusterBudget, error) {
	enforced := envBool("CUSTOM_DATABASE_POOL_BUDGET_ENFORCED", isProduction())
	maxConnections, err := optionalPositiveInt("CUSTOM_POSTGRES_MAX_CONNECTIONS")
	if err != nil {
		return ClusterBudget{}, err
	}
	if maxConnections == 0 {
		if enforced {
			return ClusterBudget{}, fmt.Errorf(
				"CUSTOM_POSTGRES_MAX_CONNECTIONS is required when database pool budget enforcement is enabled",
			)
		}
		return ClusterBudget{}, nil
	}

	defaults := map[Role]struct {
		replicas int
		maxOpen  int
	}{
		RoleAPI:              {3, 6},
		RoleParseWorker:      {2, 6},
		RoleDerivativeWorker: {2, 4},
		RoleWikiWorker:       {2, 3},
		RoleMaintenance:      {2, 2},
		RoleMigration:        {1, 1},
	}
	rows := make([]ReplicaBudget, 0, len(defaults))
	for _, role := range []Role{
		RoleAPI,
		RoleParseWorker,
		RoleDerivativeWorker,
		RoleWikiWorker,
		RoleMaintenance,
		RoleMigration,
	} {
		fallback := defaults[role]
		prefix := strings.ToUpper(strings.ReplaceAll(string(role), "-", "_"))
		replicas, err := positiveIntWithFallback(
			"CUSTOM_RUNTIME_"+prefix+"_REPLICAS",
			fallback.replicas,
		)
		if err != nil {
			return ClusterBudget{}, err
		}
		maxOpen, err := positiveIntWithFallback(
			"CUSTOM_RUNTIME_"+prefix+"_MAX_OPEN_CONNS",
			fallback.maxOpen,
		)
		if err != nil {
			return ClusterBudget{}, err
		}
		rollingFallback := 1
		if role == RoleMigration {
			rollingFallback = 0
		}
		rollingMax, err := positiveIntWithFallback(
			"CUSTOM_RUNTIME_"+prefix+"_ROLLING_MAX",
			rollingFallback,
		)
		if err != nil {
			return ClusterBudget{}, err
		}
		rows = append(rows, ReplicaBudget{
			Role: role, Replicas: replicas, MaxOpen: maxOpen, RollingMax: rollingMax,
		})
	}
	return ValidateClusterBudget(maxConnections, rows)
}

func optionalPositiveInt(key string) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, raw)
	}
	return value, nil
}

func positiveIntWithFallback(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", key, raw)
	}
	return value, nil
}
