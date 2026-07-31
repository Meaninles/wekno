package databasepool

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/Tencent/WeKnora/internal/custom/modules/runtimeprofile"
)

const (
	maxOpenConnsEnv    = "CUSTOM_DATABASE_POOL_MAX_OPEN_CONNS"
	maxIdleConnsEnv    = "CUSTOM_DATABASE_POOL_MAX_IDLE_CONNS"
	connMaxLifetimeEnv = "CUSTOM_DATABASE_POOL_CONN_MAX_LIFETIME"
	connMaxIdleTimeEnv = "CUSTOM_DATABASE_POOL_CONN_MAX_IDLE_TIME"
)

// Config bounds the database/sql pool owned by one application replica.
// Horizontal deployments must budget PostgreSQL connections per replica;
// leaving MaxOpenConns unlimited lets a burst on several replicas exhaust the
// server before queue backpressure can take effect.
type Config struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

func DefaultConfig(profiles ...runtimeprofile.Profile) Config {
	role := runtimeprofile.RoleDevAll
	if len(profiles) > 0 && profiles[0].Valid() {
		role = profiles[0].Role
	}
	maxOpen, maxIdle := 12, 4
	switch role {
	case runtimeprofile.RoleAPI:
		maxOpen, maxIdle = 6, 2
	case runtimeprofile.RoleParseWorker:
		maxOpen, maxIdle = 6, 2
	case runtimeprofile.RoleDerivativeWorker:
		maxOpen, maxIdle = 4, 1
	case runtimeprofile.RoleWikiWorker:
		maxOpen, maxIdle = 3, 1
	case runtimeprofile.RoleMaintenance:
		maxOpen, maxIdle = 2, 1
	case runtimeprofile.RoleMigration:
		maxOpen, maxIdle = 1, 1
	}
	return Config{
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    maxIdle,
		ConnMaxLifetime: 10 * time.Minute,
		ConnMaxIdleTime: 2 * time.Minute,
	}
}

// ConfigureFromEnv applies a finite, per-replica connection budget. Invalid
// explicit settings fail startup instead of silently falling back to an
// unbounded or surprising pool.
func ConfigureFromEnv(
	db *sql.DB,
	driver string,
	profiles ...runtimeprofile.Profile,
) (Config, error) {
	if db == nil {
		return Config{}, fmt.Errorf("database pool: nil database")
	}

	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "sqlite":
		cfg := Config{
			MaxOpenConns:    1,
			MaxIdleConns:    1,
			ConnMaxLifetime: 10 * time.Minute,
		}
		apply(db, cfg)
		pipelineobs.RegisterDBPool(metricRole(profiles...), db)
		return cfg, nil
	case "postgres":
		cfg, err := loadPostgresConfig(profiles...)
		if err != nil {
			return Config{}, err
		}
		apply(db, cfg)
		pipelineobs.RegisterDBPool(metricRole(profiles...), db)
		return cfg, nil
	default:
		return Config{}, fmt.Errorf("database pool: unsupported driver %q", driver)
	}
}

func metricRole(profiles ...runtimeprofile.Profile) string {
	if len(profiles) > 0 && profiles[0].Valid() {
		return string(profiles[0].Role)
	}
	return string(runtimeprofile.RoleDevAll)
}

func loadPostgresConfig(profiles ...runtimeprofile.Profile) (Config, error) {
	cfg := DefaultConfig(profiles...)
	var err error

	if cfg.MaxOpenConns, err = positiveIntEnv(maxOpenConnsEnv, cfg.MaxOpenConns); err != nil {
		return Config{}, err
	}
	if cfg.MaxIdleConns, err = positiveIntEnv(maxIdleConnsEnv, cfg.MaxIdleConns); err != nil {
		return Config{}, err
	}
	if cfg.ConnMaxLifetime, err = positiveDurationEnv(connMaxLifetimeEnv, cfg.ConnMaxLifetime); err != nil {
		return Config{}, err
	}
	if cfg.ConnMaxIdleTime, err = positiveDurationEnv(connMaxIdleTimeEnv, cfg.ConnMaxIdleTime); err != nil {
		return Config{}, err
	}
	if cfg.MaxIdleConns > cfg.MaxOpenConns {
		return Config{}, fmt.Errorf(
			"database pool: %s (%d) cannot exceed %s (%d)",
			maxIdleConnsEnv,
			cfg.MaxIdleConns,
			maxOpenConnsEnv,
			cfg.MaxOpenConns,
		)
	}
	return cfg, nil
}

func apply(db *sql.DB, cfg Config) {
	// Set the hard ceiling before the idle target so an existing pool can
	// never retain more idle connections than the new per-replica budget.
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
}

func positiveIntEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("database pool: %s must be a positive integer, got %q", name, raw)
	}
	return value, nil
}

func positiveDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("database pool: %s must be a positive duration, got %q", name, raw)
	}
	return value, nil
}
