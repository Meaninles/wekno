package iam

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

const frontendBaseURLEnv = "FRONTEND_BASE_URL"

// LoadPublicOrigin returns the canonical browser-visible origin used by IAM
// SSO. The configured application value takes precedence; the environment
// fallback keeps the module usable when Viper did not bind an env-only key.
func LoadPublicOrigin(configured string) (string, error) {
	value := strings.TrimSpace(configured)
	if value == "" {
		value = strings.TrimSpace(os.Getenv(frontendBaseURLEnv))
	}
	return normalizePublicOrigin(value)
}

func normalizePublicOrigin(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("%s is invalid: %w", frontendBaseURLEnv, err)
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%s must use http or https", frontendBaseURLEnv)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("%s must include a host", frontendBaseURLEnv)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("%s must not include user information", frontendBaseURLEnv)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", fmt.Errorf("%s must not include query or fragment", frontendBaseURLEnv)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("%s must be an origin without a path", frontendBaseURLEnv)
	}
	if scheme == "http" && !isLocalRequestHost(parsed.Host) {
		return "", fmt.Errorf("%s must use https for a non-local host", frontendBaseURLEnv)
	}

	return scheme + "://" + parsed.Host, nil
}
