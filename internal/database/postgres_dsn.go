package database

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

var postgresSSLModes = map[string]struct{}{
	"disable":     {},
	"allow":       {},
	"prefer":      {},
	"require":     {},
	"verify-ca":   {},
	"verify-full": {},
}

type postgresConnectionConfig struct {
	host        string
	port        string
	user        string
	password    string
	database    string
	sslMode     string
	sslRootCert string
	sslCert     string
	sslKey      string
}

func loadPostgresConnectionConfig() (postgresConnectionConfig, error) {
	cfg := postgresConnectionConfig{
		host:        strings.TrimSpace(os.Getenv("DB_HOST")),
		port:        strings.TrimSpace(os.Getenv("DB_PORT")),
		user:        os.Getenv("DB_USER"),
		password:    os.Getenv("DB_PASSWORD"),
		database:    strings.TrimSpace(os.Getenv("DB_NAME")),
		sslMode:     strings.ToLower(strings.TrimSpace(os.Getenv("DB_SSLMODE"))),
		sslRootCert: strings.TrimSpace(os.Getenv("DB_SSLROOTCERT")),
		sslCert:     strings.TrimSpace(os.Getenv("DB_SSLCERT")),
		sslKey:      strings.TrimSpace(os.Getenv("DB_SSLKEY")),
	}
	if cfg.sslMode == "" {
		cfg.sslMode = "disable"
	}
	if _, ok := postgresSSLModes[cfg.sslMode]; !ok {
		return postgresConnectionConfig{}, fmt.Errorf("unsupported DB_SSLMODE %q", cfg.sslMode)
	}
	if cfg.host == "" || cfg.port == "" || cfg.user == "" || cfg.database == "" {
		return postgresConnectionConfig{}, errors.New(
			"DB_HOST, DB_PORT, DB_USER and DB_NAME are required for PostgreSQL",
		)
	}
	if (cfg.sslCert == "") != (cfg.sslKey == "") {
		return postgresConnectionConfig{}, errors.New(
			"DB_SSLCERT and DB_SSLKEY must be configured together",
		)
	}
	return cfg, nil
}

func quotePostgresKeyword(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `'`, `\'`)
	return "'" + value + "'"
}

// PostgresGormDSNFromEnv builds a pgx keyword DSN without weakening TLS.
// The default remains sslmode=disable for local compatibility; production
// deployments should set DB_SSLMODE=verify-full and mount DB_SSLROOTCERT.
func PostgresGormDSNFromEnv() (string, error) {
	cfg, err := loadPostgresConnectionConfig()
	if err != nil {
		return "", err
	}
	fields := []string{
		"host=" + quotePostgresKeyword(cfg.host),
		"port=" + quotePostgresKeyword(cfg.port),
		"user=" + quotePostgresKeyword(cfg.user),
		"password=" + quotePostgresKeyword(cfg.password),
		"dbname=" + quotePostgresKeyword(cfg.database),
		"sslmode=" + quotePostgresKeyword(cfg.sslMode),
		// GORM's postgres dialector reads TimeZone itself before handing the
		// remaining keyword DSN to pgx. Unlike pgx, that small parser preserves
		// surrounding quotes and would issue SET TIME ZONE '''UTC'''. UTC has
		// no characters that require quoting, so keep this value bare.
		"TimeZone=UTC",
	}
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "sslrootcert", value: cfg.sslRootCert},
		{name: "sslcert", value: cfg.sslCert},
		{name: "sslkey", value: cfg.sslKey},
	} {
		if item.value != "" {
			fields = append(fields, item.name+"="+quotePostgresKeyword(item.value))
		}
	}
	return strings.Join(fields, " "), nil
}

// PostgresMigrationURLFromEnv builds the golang-migrate URL using net/url so
// credentials and TLS file paths containing reserved characters remain valid.
// A nil skipEmbedding omits the session option (used by version inspection).
func PostgresMigrationURLFromEnv(skipEmbedding *bool) (string, error) {
	cfg, err := loadPostgresConnectionConfig()
	if err != nil {
		return "", err
	}
	query := url.Values{"sslmode": []string{cfg.sslMode}}
	if cfg.sslRootCert != "" {
		query.Set("sslrootcert", cfg.sslRootCert)
	}
	if cfg.sslCert != "" {
		query.Set("sslcert", cfg.sslCert)
		query.Set("sslkey", cfg.sslKey)
	}
	if skipEmbedding != nil {
		query.Set("options", fmt.Sprintf("-c app.skip_embedding=%t", *skipEmbedding))
	}
	connectionURL := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.user, cfg.password),
		Host:     net.JoinHostPort(cfg.host, cfg.port),
		Path:     "/" + cfg.database,
		RawQuery: query.Encode(),
	}
	return connectionURL.String(), nil
}
