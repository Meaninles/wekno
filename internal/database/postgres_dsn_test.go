package database

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func setPostgresTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DB_HOST", "postgres.example.internal")
	t.Setenv("DB_PORT", "5432")
	t.Setenv("DB_USER", "user:name")
	t.Setenv("DB_PASSWORD", "p@ss:/?#[]")
	t.Setenv("DB_NAME", "weknora")
}

func TestPostgresDSNsPreserveTLSAndReservedCredentials(t *testing.T) {
	setPostgresTestEnv(t)
	t.Setenv("DB_SSLMODE", "verify-full")
	t.Setenv("DB_SSLROOTCERT", "/etc/db tls/ca.pem")
	t.Setenv("DB_SSLCERT", "/etc/db tls/client.pem")
	t.Setenv("DB_SSLKEY", "/etc/db tls/client.key")

	gormDSN, err := PostgresGormDSNFromEnv()
	require.NoError(t, err)
	require.Contains(t, gormDSN, "sslmode='verify-full'")
	require.Contains(t, gormDSN, "sslrootcert='/etc/db tls/ca.pem'")
	require.Contains(t, gormDSN, "TimeZone=UTC")
	require.NotContains(t, gormDSN, "TimeZone='UTC'")
	require.NotContains(t, gormDSN, "sslmode='disable'")

	skip := false
	migrationURL, err := PostgresMigrationURLFromEnv(&skip)
	require.NoError(t, err)
	parsed, err := url.Parse(migrationURL)
	require.NoError(t, err)
	password, ok := parsed.User.Password()
	require.True(t, ok)
	require.Equal(t, "p@ss:/?#[]", password)
	require.Equal(t, "verify-full", parsed.Query().Get("sslmode"))
	require.Equal(t, "/etc/db tls/ca.pem", parsed.Query().Get("sslrootcert"))
	require.Equal(t, "-c app.skip_embedding=false", parsed.Query().Get("options"))
}

func TestPostgresDSNDefaultsToLocalDisableMode(t *testing.T) {
	setPostgresTestEnv(t)
	dsn, err := PostgresGormDSNFromEnv()
	require.NoError(t, err)
	require.Contains(t, dsn, "sslmode='disable'")
}

func TestPostgresDSNRejectsUnsafeConfiguration(t *testing.T) {
	setPostgresTestEnv(t)
	t.Setenv("DB_SSLMODE", "trust-everything")
	_, err := PostgresGormDSNFromEnv()
	require.ErrorContains(t, err, "unsupported DB_SSLMODE")

	t.Setenv("DB_SSLMODE", "verify-full")
	t.Setenv("DB_SSLCERT", "/client.pem")
	t.Setenv("DB_SSLKEY", "")
	_, err = PostgresMigrationURLFromEnv(nil)
	require.ErrorContains(t, err, "must be configured together")
}

func TestPostgresKeywordEscapesQuoteAndBackslash(t *testing.T) {
	quoted := quotePostgresKeyword(`a\b'c`)
	require.True(t, strings.HasPrefix(quoted, "'"))
	require.Equal(t, `'a\\b\'c'`, quoted)
}
