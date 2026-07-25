// Package workloadbudget centralizes per-document fan-out bounds. Limits are
// evaluated before durable task plans are persisted, so retries replay the
// same bounded work instead of recalculating from mutable configuration.
package workloadbudget

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	MaxQuestionChunks          int
	MaxGeneratedQuestions      int
	MaxGraphChunks             int
	GraphBatchSize             int
	MaxDownstreamTasks         int
	MaxMultimodalImages        int
	QuestionFailureRatioPermil int
	WikiFailureRatioPermil     int
}

func FromEnv() Config {
	return Config{
		MaxQuestionChunks:          envInt("CUSTOM_DOCUMENT_BUDGET_QUESTION_CHUNKS", 256),
		MaxGeneratedQuestions:      envInt("CUSTOM_DOCUMENT_BUDGET_GENERATED_QUESTIONS", 512),
		MaxGraphChunks:             envInt("CUSTOM_DOCUMENT_BUDGET_GRAPH_CHUNKS", 512),
		GraphBatchSize:             envInt("CUSTOM_DOCUMENT_BUDGET_GRAPH_BATCH_SIZE", 8),
		MaxDownstreamTasks:         envInt("CUSTOM_DOCUMENT_BUDGET_DOWNSTREAM_TASKS", 256),
		MaxMultimodalImages:        envInt("CUSTOM_DOCUMENT_BUDGET_MULTIMODAL_IMAGES", 100),
		QuestionFailureRatioPermil: envInt("CUSTOM_DOCUMENT_BUDGET_QUESTION_FAILURE_PERMIL", 250),
		WikiFailureRatioPermil:     envInt("CUSTOM_DOCUMENT_BUDGET_WIKI_FAILURE_PERMIL", 200),
	}
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func (c Config) QuestionChunkCap(questionCount int) int {
	cap := c.MaxQuestionChunks
	if cap <= 0 {
		return 0
	}
	if questionCount <= 0 {
		questionCount = 1
	}
	if c.MaxGeneratedQuestions > 0 {
		byOutputs := c.MaxGeneratedQuestions / questionCount
		if byOutputs < 1 {
			byOutputs = 1
		}
		if byOutputs < cap {
			cap = byOutputs
		}
	}
	return cap
}

func (c Config) GraphTaskCap(summaryTasks, questionBatchTasks int) int {
	cap := c.MaxGraphChunks
	if cap <= 0 {
		return 0
	}
	batchSize := c.GraphBatchSize
	if batchSize <= 0 {
		batchSize = 1
	}
	if c.MaxDownstreamTasks > 0 {
		remaining := c.MaxDownstreamTasks - summaryTasks - questionBatchTasks
		if remaining < 0 {
			remaining = 0
		}
		byTasks := remaining * batchSize
		if byTasks < cap {
			cap = byTasks
		}
	}
	return cap
}

func (c Config) GraphTaskCount(sourceChunks int) int {
	if sourceChunks <= 0 {
		return 0
	}
	batchSize := c.GraphBatchSize
	if batchSize <= 0 {
		batchSize = 1
	}
	return (sourceChunks + batchSize - 1) / batchSize
}

func (c Config) QuestionFailureExceeded(failed, attempted int) bool {
	return failureRatioExceeded(failed, attempted, c.QuestionFailureRatioPermil)
}

func (c Config) WikiFailureExceeded(failed, attempted int) bool {
	return failureRatioExceeded(failed, attempted, c.WikiFailureRatioPermil)
}

func failureRatioExceeded(failed, attempted, threshold int) bool {
	if failed <= 0 || attempted <= 0 {
		return false
	}
	if threshold <= 0 {
		threshold = 1
	}
	return failed*1000 >= attempted*threshold
}

// Stratified returns at most limit entries while retaining endpoints and
// stable document order. It is deterministic for a durable fan-out plan.
func Stratified[T any](items []T, limit int) []T {
	if limit <= 0 || len(items) == 0 {
		return nil
	}
	if len(items) <= limit {
		return append([]T(nil), items...)
	}
	if limit == 1 {
		return []T{items[len(items)/2]}
	}
	result := make([]T, 0, limit)
	previous := -1
	for position := 0; position < limit; position++ {
		index := position * (len(items) - 1) / (limit - 1)
		if index == previous {
			continue
		}
		result = append(result, items[index])
		previous = index
	}
	return result
}
