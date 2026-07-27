package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/workretry"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

func TestProviderOutageIsRetryBudgetFreeWithDeterministicJitter(t *testing.T) {
	rawErr := errors.New("upstream 503")
	providerErr := &modeladmission.ProviderUnavailableError{
		Kind:       modeladmission.KindChat,
		RetryAfter: 20 * time.Second,
		Cause:      rawErr,
	}
	require.False(t, asynqIsFailureFunc(providerErr))

	task := asynq.NewTask(
		types.TypeQuestionGeneration,
		[]byte(`{"knowledge_id":"knowledge-1","batch_index":7}`),
	)
	first := asynqRetryDelayFunc(3, providerErr, task)
	second := asynqRetryDelayFunc(99, providerErr, task)
	require.Equal(t, first, second)
	require.GreaterOrEqual(t, first, 20*time.Second)
	require.Less(t, first, 20*time.Second+modeladmission.ProviderRetrySpreadWindow(20*time.Second))
	require.ErrorIs(t, providerErr, rawErr)
}

func TestCircuitRejectIsRetryBudgetFreeButSemanticFailureIsNot(t *testing.T) {
	circuitErr := &modeladmission.CircuitOpenError{
		Kind:       modeladmission.KindVLM,
		RetryAfter: 45 * time.Second,
	}
	require.False(t, asynqIsFailureFunc(circuitErr))
	delay := asynqRetryDelayFunc(
		0,
		circuitErr,
		asynq.NewTask(types.TypeImageMultimodal, []byte(`{"knowledge_id":"k"}`)),
	)
	require.GreaterOrEqual(t, delay, 45*time.Second)
	require.Less(t, delay, 45*time.Second+modeladmission.ProviderRetrySpreadWindow(45*time.Second))

	semanticErr := errors.New("decode graph JSON: invalid character")
	require.True(t, asynqIsFailureFunc(semanticErr))
}

func TestRealProviderFailureConsumesBoundedWorkRetryBudget(t *testing.T) {
	providerErr := &modeladmission.ProviderUnavailableError{
		Kind:       modeladmission.KindVLM,
		RetryAfter: 30 * time.Second,
		Cause:      errors.New("upstream request timed out"),
	}
	budgeted := workretry.ConsumeProviderFailure(providerErr)

	require.True(t, asynqIsFailureFunc(budgeted))
	delay := asynqRetryDelayFunc(
		0,
		budgeted,
		asynq.NewTask(types.TypeImageMultimodal, []byte(`{"knowledge_id":"k"}`)),
	)
	require.GreaterOrEqual(t, delay, 30*time.Second)
}

func TestShutdownCancellationAndAdmissionBackendFailureAreBudgetFree(t *testing.T) {
	task := asynq.NewTask(types.TypeSummaryGeneration, []byte(`{"knowledge_id":"k"}`))
	require.False(t, asynqIsFailureFunc(context.Canceled))
	require.Equal(t, documentOwnershipConflictRetryDelay,
		asynqRetryDelayFunc(3, context.Canceled, task))

	admissionErr := errors.Join(
		modeladmission.ErrAdmissionBackendUnavailable,
		errors.New("redis i/o timeout"),
	)
	require.False(t, asynqIsFailureFunc(admissionErr))
	delay := asynqRetryDelayFunc(3, admissionErr, task)
	require.GreaterOrEqual(t, delay, 15*time.Second)
	require.Less(t, delay, 15*time.Second+modeladmission.ProviderRetrySpreadWindow(15*time.Second))
}
