package answerfeedback

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	QueueSize           int
	MaxRetries          int
	SnapshotDelay       time.Duration
	SnapshotJitter      time.Duration
	SnapshotNightWindow string
}

func LoadConfigFromEnv() Config {
	return Config{
		QueueSize:           envInt("CUSTOM_ANSWER_FEEDBACK_QUEUE_SIZE", 512),
		MaxRetries:          envInt("CUSTOM_ANSWER_FEEDBACK_MAX_RETRIES", 2),
		SnapshotDelay:       envDuration("CUSTOM_ANSWER_FEEDBACK_SNAPSHOT_DELAY", 90*time.Second),
		SnapshotJitter:      envDuration("CUSTOM_ANSWER_FEEDBACK_SNAPSHOT_JITTER", 180*time.Second),
		SnapshotNightWindow: strings.TrimSpace(os.Getenv("CUSTOM_ANSWER_FEEDBACK_SNAPSHOT_NIGHT_WINDOW")),
	}
}

func (c Config) normalize() Config {
	if c.QueueSize <= 0 {
		c.QueueSize = 512
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.SnapshotDelay < 0 {
		c.SnapshotDelay = 0
	}
	if c.SnapshotJitter < 0 {
		c.SnapshotJitter = 0
	}
	return c
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
