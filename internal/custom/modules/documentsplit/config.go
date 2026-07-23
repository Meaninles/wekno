package documentsplit

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TargetRatio       float64
	MaxExpansionRatio float64
	PartConcurrency   int
	PerDocumentWindow int
	LeaseDuration     time.Duration
	TaskTimeout       time.Duration
	RecoveryInterval  time.Duration
	RetryBackoffBase  time.Duration
	RetryBackoffMax   time.Duration
	RecoveryBatchSize int
	MaxRetry          int
	ArchiveMaxParts   int
	FinalizeBatchSize int
	// Post-processing every row-level chunk of a very large logical document
	// creates thousands of repetitive LLM calls and a noisy graph/question
	// corpus. These bounds are applied as deterministic logical strata; small
	// split documents still enrich every chunk.
	QuestionStrata      int
	GraphStrata         int
	TableQuestionStrata int
	TableGraphStrata    int
}

func LoadConfig() Config {
	cfg := Config{
		TargetRatio:         0.75,
		MaxExpansionRatio:   12,
		PartConcurrency:     4,
		PerDocumentWindow:   4,
		LeaseDuration:       2 * time.Minute,
		TaskTimeout:         30 * time.Minute,
		RecoveryInterval:    10 * time.Second,
		RetryBackoffBase:    30 * time.Second,
		RetryBackoffMax:     5 * time.Minute,
		RecoveryBatchSize:   200,
		MaxRetry:            6,
		ArchiveMaxParts:     10_000,
		FinalizeBatchSize:   100,
		QuestionStrata:      256,
		GraphStrata:         512,
		TableQuestionStrata: 64,
		TableGraphStrata:    128,
	}
	cfg.TargetRatio = floatEnv("CUSTOM_DOCUMENT_SPLIT_TARGET_RATIO", cfg.TargetRatio)
	cfg.MaxExpansionRatio = floatEnv(
		"CUSTOM_DOCUMENT_SPLIT_MAX_EXPANSION_RATIO", cfg.MaxExpansionRatio,
	)
	cfg.PartConcurrency = intEnv("CUSTOM_DOCUMENT_SPLIT_WORKER_CONCURRENCY", cfg.PartConcurrency)
	cfg.PerDocumentWindow = intEnv("CUSTOM_DOCUMENT_SPLIT_PER_DOCUMENT_WINDOW", cfg.PerDocumentWindow)
	cfg.LeaseDuration = durationEnv("CUSTOM_DOCUMENT_SPLIT_LEASE_DURATION", cfg.LeaseDuration)
	cfg.TaskTimeout = durationEnv("CUSTOM_DOCUMENT_SPLIT_TASK_TIMEOUT", cfg.TaskTimeout)
	cfg.RecoveryInterval = durationEnv("CUSTOM_DOCUMENT_SPLIT_RECOVERY_INTERVAL", cfg.RecoveryInterval)
	cfg.RetryBackoffBase = durationEnv(
		"CUSTOM_DOCUMENT_SPLIT_RETRY_BACKOFF_BASE", cfg.RetryBackoffBase,
	)
	cfg.RetryBackoffMax = durationEnv(
		"CUSTOM_DOCUMENT_SPLIT_RETRY_BACKOFF_MAX", cfg.RetryBackoffMax,
	)
	cfg.RecoveryBatchSize = intEnv("CUSTOM_DOCUMENT_SPLIT_RECOVERY_BATCH_SIZE", cfg.RecoveryBatchSize)
	cfg.MaxRetry = intEnv("CUSTOM_DOCUMENT_SPLIT_MAX_RETRY", cfg.MaxRetry)
	cfg.ArchiveMaxParts = intEnv("CUSTOM_DOCUMENT_SPLIT_MAX_PARTS", cfg.ArchiveMaxParts)
	cfg.FinalizeBatchSize = intEnv("CUSTOM_DOCUMENT_SPLIT_FINALIZE_BATCH_SIZE", cfg.FinalizeBatchSize)
	cfg.QuestionStrata = intEnv("CUSTOM_DOCUMENT_SPLIT_QUESTION_STRATA", cfg.QuestionStrata)
	cfg.GraphStrata = intEnv("CUSTOM_DOCUMENT_SPLIT_GRAPH_STRATA", cfg.GraphStrata)
	cfg.TableQuestionStrata = intEnv(
		"CUSTOM_DOCUMENT_SPLIT_TABLE_QUESTION_STRATA", cfg.TableQuestionStrata,
	)
	cfg.TableGraphStrata = intEnv(
		"CUSTOM_DOCUMENT_SPLIT_TABLE_GRAPH_STRATA", cfg.TableGraphStrata,
	)
	if cfg.TargetRatio < 0.5 || cfg.TargetRatio > 0.9 {
		cfg.TargetRatio = 0.75
	}
	if cfg.PerDocumentWindow > cfg.PartConcurrency {
		cfg.PerDocumentWindow = cfg.PartConcurrency
	}
	return cfg.normalized()
}

func (cfg Config) normalized() Config {
	defaults := Config{
		TargetRatio: 0.75, MaxExpansionRatio: 12,
		PartConcurrency: 4, PerDocumentWindow: 4,
		LeaseDuration: 2 * time.Minute, TaskTimeout: 30 * time.Minute,
		RecoveryInterval: 10 * time.Second,
		RetryBackoffBase: 30 * time.Second, RetryBackoffMax: 5 * time.Minute,
		RecoveryBatchSize: 200, MaxRetry: 6, ArchiveMaxParts: 10_000,
		FinalizeBatchSize: 100,
		QuestionStrata:    256, GraphStrata: 512,
		TableQuestionStrata: 64, TableGraphStrata: 128,
	}
	if cfg.TargetRatio < 0.5 || cfg.TargetRatio > 0.9 {
		cfg.TargetRatio = defaults.TargetRatio
	}
	if cfg.MaxExpansionRatio < 1 || cfg.MaxExpansionRatio > 100 {
		cfg.MaxExpansionRatio = defaults.MaxExpansionRatio
	}
	if cfg.PartConcurrency <= 0 {
		cfg.PartConcurrency = defaults.PartConcurrency
	}
	if cfg.PerDocumentWindow <= 0 {
		cfg.PerDocumentWindow = defaults.PerDocumentWindow
	}
	if cfg.PerDocumentWindow > cfg.PartConcurrency {
		cfg.PerDocumentWindow = cfg.PartConcurrency
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = defaults.LeaseDuration
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = defaults.TaskTimeout
	}
	// Healthy long-running parsers renew a short failure-detection lease. The
	// independent task deadline must never expire before that renewable lease.
	if cfg.TaskTimeout <= cfg.LeaseDuration {
		cfg.TaskTimeout = 2 * cfg.LeaseDuration
	}
	if cfg.RecoveryInterval <= 0 {
		cfg.RecoveryInterval = defaults.RecoveryInterval
	}
	if cfg.RetryBackoffBase <= 0 {
		cfg.RetryBackoffBase = defaults.RetryBackoffBase
	}
	if cfg.RetryBackoffMax < cfg.RetryBackoffBase {
		cfg.RetryBackoffMax = cfg.RetryBackoffBase
	}
	if cfg.RecoveryBatchSize <= 0 || cfg.RecoveryBatchSize > 1000 {
		cfg.RecoveryBatchSize = defaults.RecoveryBatchSize
	}
	if cfg.MaxRetry <= 0 {
		cfg.MaxRetry = defaults.MaxRetry
	}
	if cfg.ArchiveMaxParts <= 0 {
		cfg.ArchiveMaxParts = defaults.ArchiveMaxParts
	}
	if cfg.FinalizeBatchSize <= 0 || cfg.FinalizeBatchSize > 1000 {
		cfg.FinalizeBatchSize = defaults.FinalizeBatchSize
	}
	if cfg.QuestionStrata <= 0 || cfg.QuestionStrata > 10_000 {
		cfg.QuestionStrata = defaults.QuestionStrata
	}
	if cfg.GraphStrata <= 0 || cfg.GraphStrata > 10_000 {
		cfg.GraphStrata = defaults.GraphStrata
	}
	if cfg.TableQuestionStrata <= 0 {
		cfg.TableQuestionStrata = defaults.TableQuestionStrata
	}
	if cfg.TableQuestionStrata > cfg.QuestionStrata {
		cfg.TableQuestionStrata = cfg.QuestionStrata
	}
	if cfg.TableGraphStrata <= 0 {
		cfg.TableGraphStrata = defaults.TableGraphStrata
	}
	if cfg.TableGraphStrata > cfg.GraphStrata {
		cfg.TableGraphStrata = cfg.GraphStrata
	}
	return cfg
}

func intEnv(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func floatEnv(name string, fallback float64) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
