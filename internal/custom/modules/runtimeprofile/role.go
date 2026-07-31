package runtimeprofile

import (
	"fmt"
	"os"
	"strings"
)

type Role string

const (
	RoleAPI              Role = "api"
	RoleParseWorker      Role = "parse-worker"
	RoleDerivativeWorker Role = "derivative-worker"
	RoleWikiWorker       Role = "wiki-worker"
	RoleMaintenance      Role = "maintenance"
	RoleMigration        Role = "migration"
	RoleDevAll           Role = "dev-all"
)

const (
	roleEnv        = "WEKNORA_RUNTIME_ROLE"
	enforcedEnv    = "CUSTOM_RUNTIME_ROLE_ENFORCED"
	environmentEnv = "CUSTOM_RUNTIME_ENV"
)

// Profile is the immutable capability set for one process. Capability checks
// deliberately live here so container registrations cannot drift into a
// second, inconsistent role matrix.
type Profile struct {
	Role Role
}

func LoadFromEnv() (Profile, error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(roleEnv)))
	production := isProduction()
	enforced := envBool(enforcedEnv, production)
	if raw == "" {
		if enforced {
			return Profile{}, fmt.Errorf("%s is required when runtime role enforcement is enabled", roleEnv)
		}
		raw = string(RoleDevAll)
	}

	role := Role(raw)
	switch role {
	case RoleAPI, RoleParseWorker, RoleDerivativeWorker, RoleWikiWorker,
		RoleMaintenance, RoleMigration:
	case RoleDevAll:
		if production {
			return Profile{}, fmt.Errorf("%s=%s is forbidden in production", roleEnv, role)
		}
	default:
		return Profile{}, fmt.Errorf("unsupported %s=%q", roleEnv, raw)
	}
	return Profile{Role: role}, nil
}

func MustLoadFromEnv() Profile {
	profile, err := LoadFromEnv()
	if err != nil {
		panic(err)
	}
	return profile
}

func (p Profile) Valid() bool {
	switch p.Role {
	case RoleAPI, RoleParseWorker, RoleDerivativeWorker, RoleWikiWorker,
		RoleMaintenance, RoleMigration, RoleDevAll:
		return true
	default:
		return false
	}
}

func (p Profile) ServesAPI() bool {
	return p.Role == RoleAPI || p.Role == RoleDevAll
}

func (p Profile) RunsParseWorker() bool {
	return p.Role == RoleParseWorker || p.Role == RoleDevAll
}

func (p Profile) RunsDerivativeWorker() bool {
	return p.Role == RoleDerivativeWorker || p.Role == RoleDevAll
}

func (p Profile) RunsWikiWorker() bool {
	return p.Role == RoleWikiWorker || p.Role == RoleDevAll
}

func (p Profile) RunsMaintenance() bool {
	return p.Role == RoleMaintenance || p.Role == RoleDevAll
}

func (p Profile) RunsMigration() bool {
	return p.Role == RoleMigration || p.Role == RoleDevAll
}

func (p Profile) RunsAnyWorker() bool {
	return p.RunsParseWorker() || p.RunsDerivativeWorker() || p.RunsWikiWorker() ||
		p.RunsMaintenance()
}

func (p Profile) StartsSchedulers() bool {
	return p.RunsMaintenance()
}

func isProduction() bool {
	for _, key := range []string{environmentEnv, "WEKNORA_ENVIRONMENT", "APP_ENV"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
		case "production", "prod":
			return true
		}
	}
	return false
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
