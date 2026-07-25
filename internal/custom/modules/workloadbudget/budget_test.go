package workloadbudget

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStratifiedIsStableAndRetainsEndpoints(t *testing.T) {
	input := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	require.Equal(t, []int{0, 3, 6, 9}, Stratified(input, 4))
	require.Equal(t, []int{5}, Stratified(input, 1))
	require.Nil(t, Stratified(input, 0))
}

func TestQuestionAndTaskBudgetsCompose(t *testing.T) {
	config := Config{
		MaxQuestionChunks:     256,
		MaxGeneratedQuestions: 512,
		MaxGraphChunks:        512,
		GraphBatchSize:        8,
		MaxDownstreamTasks:    100,
	}
	require.Equal(t, 170, config.QuestionChunkCap(3))
	require.Equal(t, 512, config.GraphTaskCap(1, 10))
	require.Equal(t, 64, config.GraphTaskCount(512))
}

func TestGraphBudgetCapsSourceChunksByAvailableBatchTasks(t *testing.T) {
	config := Config{
		MaxGraphChunks:     512,
		GraphBatchSize:     8,
		MaxDownstreamTasks: 10,
	}
	require.Equal(t, 64, config.GraphTaskCap(1, 1))
	require.Equal(t, 8, config.GraphTaskCount(64))
	require.Equal(t, 9, config.GraphTaskCount(65))
}

func TestQuestionFailureThreshold(t *testing.T) {
	config := Config{QuestionFailureRatioPermil: 250}
	require.False(t, config.QuestionFailureExceeded(1, 5))
	require.True(t, config.QuestionFailureExceeded(1, 4))
	require.True(t, config.QuestionFailureExceeded(4, 4))
}

func TestWikiFailureThreshold(t *testing.T) {
	config := Config{WikiFailureRatioPermil: 200}
	require.False(t, config.WikiFailureExceeded(1, 6))
	require.True(t, config.WikiFailureExceeded(1, 5))
}
