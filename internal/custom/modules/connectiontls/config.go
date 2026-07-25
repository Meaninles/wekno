package connectiontls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func envBool(name string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return value, nil
}

// RedisConfigFromEnv builds a TLS 1.2+ client configuration for Redis.
// It is nil when REDIS_TLS_ENABLED is false. A custom CA and mTLS identity are
// optional; the system trust store remains available when no CA file is set.
func RedisConfigFromEnv() (*tls.Config, error) {
	enabled, err := envBool("REDIS_TLS_ENABLED")
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	insecure, err := envBool("REDIS_TLS_INSECURE_SKIP_VERIFY")
	if err != nil {
		return nil, err
	}
	caFile := strings.TrimSpace(os.Getenv("REDIS_TLS_CA_FILE"))
	certFile := strings.TrimSpace(os.Getenv("REDIS_TLS_CERT_FILE"))
	keyFile := strings.TrimSpace(os.Getenv("REDIS_TLS_KEY_FILE"))
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("REDIS_TLS_CERT_FILE and REDIS_TLS_KEY_FILE must be configured together")
	}

	config := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         strings.TrimSpace(os.Getenv("REDIS_TLS_SERVER_NAME")),
		InsecureSkipVerify: insecure, // #nosec G402 -- explicit break-glass setting, false by default.
	}
	if caFile != "" {
		caPEM, readErr := os.ReadFile(caFile)
		if readErr != nil {
			return nil, fmt.Errorf("read REDIS_TLS_CA_FILE: %w", readErr)
		}
		roots, poolErr := x509.SystemCertPool()
		if poolErr != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if ok := roots.AppendCertsFromPEM(caPEM); !ok {
			return nil, errors.New("REDIS_TLS_CA_FILE contains no valid CA certificate")
		}
		config.RootCAs = roots
	}
	if certFile != "" {
		certificate, loadErr := tls.LoadX509KeyPair(certFile, keyFile)
		if loadErr != nil {
			return nil, fmt.Errorf("load Redis TLS client certificate: %w", loadErr)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}
