package connectiontls

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedisConfigDisabledByDefault(t *testing.T) {
	t.Setenv("REDIS_TLS_ENABLED", "")
	config, err := RedisConfigFromEnv()
	require.NoError(t, err)
	require.Nil(t, config)
}

func TestRedisConfigRequiresTLS12AndServerVerification(t *testing.T) {
	t.Setenv("REDIS_TLS_ENABLED", "true")
	t.Setenv("REDIS_TLS_SERVER_NAME", "redis.example.internal")
	config, err := RedisConfigFromEnv()
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS12), config.MinVersion)
	require.Equal(t, "redis.example.internal", config.ServerName)
	require.False(t, config.InsecureSkipVerify)
}

func TestRedisConfigRejectsPartialMTLSAndInvalidCA(t *testing.T) {
	t.Setenv("REDIS_TLS_ENABLED", "true")
	t.Setenv("REDIS_TLS_CERT_FILE", "/client.pem")
	_, err := RedisConfigFromEnv()
	require.ErrorContains(t, err, "must be configured together")

	t.Setenv("REDIS_TLS_CERT_FILE", "")
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte("not a certificate"), 0o600))
	t.Setenv("REDIS_TLS_CA_FILE", path)
	_, err = RedisConfigFromEnv()
	require.ErrorContains(t, err, "contains no valid CA")
}

func TestRedisConfigRejectsInvalidBoolean(t *testing.T) {
	t.Setenv("REDIS_TLS_ENABLED", "sometimes")
	_, err := RedisConfigFromEnv()
	require.ErrorContains(t, err, "must be a boolean")
}
