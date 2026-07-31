package databasepool

import (
	"database/sql"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/runtimeprofile"
	_ "github.com/mattn/go-sqlite3"
)

func TestConfigurePostgresDefaultsBoundEveryReplica(t *testing.T) {
	clearPoolEnv(t)
	db := openSQLiteForPoolTest(t)

	cfg, err := ConfigureFromEnv(db, "postgres")
	if err != nil {
		t.Fatalf("ConfigureFromEnv() error = %v", err)
	}
	if cfg != DefaultConfig() {
		t.Fatalf("config = %+v, want %+v", cfg, DefaultConfig())
	}
	if got := db.Stats().MaxOpenConnections; got != 12 {
		t.Fatalf("MaxOpenConnections = %d, want 12", got)
	}
}

func TestRoleDefaultsMatchClusterBudget(t *testing.T) {
	clearPoolEnv(t)
	tests := []struct {
		role runtimeprofile.Role
		open int
		idle int
	}{
		{runtimeprofile.RoleAPI, 6, 2},
		{runtimeprofile.RoleParseWorker, 6, 2},
		{runtimeprofile.RoleDerivativeWorker, 4, 1},
		{runtimeprofile.RoleWikiWorker, 3, 1},
		{runtimeprofile.RoleMaintenance, 2, 1},
		{runtimeprofile.RoleMigration, 1, 1},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			db := openSQLiteForPoolTest(t)
			cfg, err := ConfigureFromEnv(
				db,
				"postgres",
				runtimeprofile.Profile{Role: tt.role},
			)
			if err != nil {
				t.Fatalf("ConfigureFromEnv() error = %v", err)
			}
			if cfg.MaxOpenConns != tt.open || cfg.MaxIdleConns != tt.idle {
				t.Fatalf("config = %+v, want open=%d idle=%d", cfg, tt.open, tt.idle)
			}
		})
	}
}

func TestConfigurePostgresHonorsExplicitBudget(t *testing.T) {
	clearPoolEnv(t)
	t.Setenv(maxOpenConnsEnv, "8")
	t.Setenv(maxIdleConnsEnv, "2")
	t.Setenv(connMaxLifetimeEnv, "7m")
	t.Setenv(connMaxIdleTimeEnv, "45s")
	db := openSQLiteForPoolTest(t)

	cfg, err := ConfigureFromEnv(db, "postgres")
	if err != nil {
		t.Fatalf("ConfigureFromEnv() error = %v", err)
	}
	want := Config{
		MaxOpenConns:    8,
		MaxIdleConns:    2,
		ConnMaxLifetime: 7 * time.Minute,
		ConnMaxIdleTime: 45 * time.Second,
	}
	if cfg != want {
		t.Fatalf("config = %+v, want %+v", cfg, want)
	}
	if got := db.Stats().MaxOpenConnections; got != 8 {
		t.Fatalf("MaxOpenConnections = %d, want 8", got)
	}
}

func TestConfigureRejectsUnsafeExplicitSettings(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero open", key: maxOpenConnsEnv, value: "0"},
		{name: "invalid idle", key: maxIdleConnsEnv, value: "many"},
		{name: "zero lifetime", key: connMaxLifetimeEnv, value: "0s"},
		{name: "invalid idle time", key: connMaxIdleTimeEnv, value: "soon"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearPoolEnv(t)
			t.Setenv(tt.key, tt.value)
			db := openSQLiteForPoolTest(t)
			if _, err := ConfigureFromEnv(db, "postgres"); err == nil {
				t.Fatalf("ConfigureFromEnv() accepted %s=%q", tt.key, tt.value)
			}
		})
	}
}

func TestConfigureRejectsIdleAboveOpen(t *testing.T) {
	clearPoolEnv(t)
	t.Setenv(maxOpenConnsEnv, "4")
	t.Setenv(maxIdleConnsEnv, "5")
	db := openSQLiteForPoolTest(t)
	if _, err := ConfigureFromEnv(db, "postgres"); err == nil {
		t.Fatal("ConfigureFromEnv() accepted MaxIdleConns above MaxOpenConns")
	}
}

func TestConfigureSQLiteKeepsSingleWriter(t *testing.T) {
	clearPoolEnv(t)
	t.Setenv(maxOpenConnsEnv, "32")
	db := openSQLiteForPoolTest(t)
	cfg, err := ConfigureFromEnv(db, "sqlite")
	if err != nil {
		t.Fatalf("ConfigureFromEnv() error = %v", err)
	}
	if cfg.MaxOpenConns != 1 || db.Stats().MaxOpenConnections != 1 {
		t.Fatalf("SQLite pool = %+v stats=%+v, want one open connection", cfg, db.Stats())
	}
}

func openSQLiteForPoolTest(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func clearPoolEnv(t *testing.T) {
	t.Helper()
	t.Setenv(maxOpenConnsEnv, "")
	t.Setenv(maxIdleConnsEnv, "")
	t.Setenv(connMaxLifetimeEnv, "")
	t.Setenv(connMaxIdleTimeEnv, "")
}
