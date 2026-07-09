package authsecurity

import (
	"os"
	"strconv"
	"time"
)

const (
	defaultChallengeTTLSeconds = 300
	defaultMaxFailures         = 5
	defaultLockMinutes         = 15
	defaultFailureWindowMin    = 15
)

type Config struct {
	ChallengeTTL  time.Duration
	MaxFailures   int
	LockDuration  time.Duration
	FailureWindow time.Duration
}

func LoadConfigFromEnv() Config {
	return Config{
		ChallengeTTL:  time.Duration(envInt("CUSTOM_AUTH_SECURITY_CHALLENGE_TTL_SECONDS", defaultChallengeTTLSeconds)) * time.Second,
		MaxFailures:   envInt("CUSTOM_AUTH_SECURITY_MAX_FAILURES", defaultMaxFailures),
		LockDuration:  time.Duration(envInt("CUSTOM_AUTH_SECURITY_LOCK_MINUTES", defaultLockMinutes)) * time.Minute,
		FailureWindow: time.Duration(envInt("CUSTOM_AUTH_SECURITY_FAILURE_WINDOW_MINUTES", defaultFailureWindowMin)) * time.Minute,
	}.normalize()
}

func (c Config) normalize() Config {
	if c.ChallengeTTL <= 0 {
		c.ChallengeTTL = defaultChallengeTTLSeconds * time.Second
	}
	if c.MaxFailures <= 0 {
		c.MaxFailures = defaultMaxFailures
	}
	if c.LockDuration <= 0 {
		c.LockDuration = defaultLockMinutes * time.Minute
	}
	if c.FailureWindow <= 0 {
		c.FailureWindow = defaultFailureWindowMin * time.Minute
	}
	return c
}

func envInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
