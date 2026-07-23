package documentqueue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

type fakeRuntimeTerminationVerifier struct {
	calls    atomic.Int32
	evidence RuntimeTerminationEvidence
	err      error
}

func (f *fakeRuntimeTerminationVerifier) VerifyTermination(
	_ context.Context,
	_ string,
	_ string,
) (RuntimeTerminationEvidence, error) {
	f.calls.Add(1)
	return f.evidence, f.err
}

func TestKubernetesRuntimeRecoveryRequiresEveryTakeoverGate(t *testing.T) {
	ownerID := "k8s/runtime-ns/pod-uid-old"
	owner := newQueueTestCoordinator(t, ownerID, "boot-old", 1)
	payload := workflowPayload(t, 1701, "knowledge-runtime-proof", "generation-1")
	workflow, _, err := owner.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	require.NoError(t, err)
	bindWorkflowForTest(t, owner, workflow)
	_, err = owner.Claim(context.Background(), workflow.TaskType, deliveryPayload(t, workflow))
	require.NoError(t, err)
	require.NoError(t, owner.db.Where("id = ?", workflow.ID).Take(workflow).Error)

	other := NewCoordinatorWithConfig(
		owner.db, nil, "k8s/runtime-ns/pod-uid-new", "boot-new", 1, owner.config,
	)
	require.NoError(t, other.registerAndAdopt(context.Background()))
	require.NoError(t, other.MarkReady(context.Background()))
	verifier := &fakeRuntimeTerminationVerifier{}
	other.runtimeVerifier = verifier

	now := time.Now()
	// 1. An unexpired workflow lease prevents even asking Kubernetes.
	workflow.LeaseUntil = pointerTime(now.Add(time.Minute))
	proven, err := other.confirmForeignRuntimeTermination(context.Background(), workflow, now)
	require.NoError(t, err)
	require.False(t, proven)
	require.Zero(t, verifier.calls.Load())

	// 2. Lease expiry alone is not enough while the owner heartbeat is fresh.
	workflow.LeaseUntil = pointerTime(now.Add(-time.Second))
	require.NoError(t, owner.db.Model(&Workflow{}).Where("id = ?", workflow.ID).
		Update("lease_until", *workflow.LeaseUntil).Error)
	proven, err = other.confirmForeignRuntimeTermination(context.Background(), workflow, now)
	require.NoError(t, err)
	require.False(t, proven)
	require.Zero(t, verifier.calls.Load())

	// 3. A stale heartbeat allows an exact runtime lookup, but a negative
	// verdict (including deletionTimestamp/404 in the real verifier) keeps the
	// owner live and the workflow leased.
	require.NoError(t, owner.db.Model(&Instance{}).Where("instance_id = ?", ownerID).
		Update("last_heartbeat_at", now.Add(-3*owner.config.InstanceStaleAfter)).Error)
	verifier.evidence = RuntimeTerminationEvidence{Reason: "pod_uid_not_present_is_not_proof"}
	proven, err = other.confirmForeignRuntimeTermination(context.Background(), workflow, now)
	require.NoError(t, err)
	require.False(t, proven)
	require.EqualValues(t, 1, verifier.calls.Load())
	var instance Instance
	require.NoError(t, owner.db.Where("instance_id = ?", ownerID).Take(&instance).Error)
	require.NotEqual(t, InstanceStopped, instance.State)

	// 4. Only an affirmative exact Pod/container terminal state changes the
	// exact old boot to stopped. The existing workflow CAS can then reclaim it.
	verifier.evidence = RuntimeTerminationEvidence{
		Proven: true,
		Proof:  "kubernetes:container_terminated:runtime-ns/parser-old:pod-uid-old:container=app",
		Reason: "app_container_terminated",
	}
	proven, err = other.confirmForeignRuntimeTermination(context.Background(), workflow, now)
	require.NoError(t, err)
	require.True(t, proven)
	require.EqualValues(t, 2, verifier.calls.Load())
	require.NoError(t, owner.db.Where("instance_id = ?", ownerID).Take(&instance).Error)
	require.Equal(t, InstanceStopped, instance.State)
	reclaimed, err := other.requeueExpired(context.Background(), workflow, now)
	require.NoError(t, err)
	require.NotNil(t, reclaimed)
	require.Equal(t, StateQueued, reclaimed.State)
	require.EqualValues(t, workflow.DispatchEpoch+1, reclaimed.DispatchEpoch)
}

func TestKubernetesRuntimeRecoveryFailsClosedOnVerifierError(t *testing.T) {
	ownerID := "k8s/runtime-ns/pod-uid-error"
	owner := newQueueTestCoordinator(t, ownerID, "boot-error", 1)
	other := NewCoordinatorWithConfig(
		owner.db, nil, "k8s/runtime-ns/pod-uid-survivor", "boot-survivor", 1, owner.config,
	)
	require.NoError(t, other.registerAndAdopt(context.Background()))
	require.NoError(t, other.MarkReady(context.Background()))
	other.runtimeVerifier = &fakeRuntimeTerminationVerifier{err: errors.New("Kubernetes API unavailable")}
	now := time.Now()
	require.NoError(t, owner.db.Model(&Instance{}).Where("instance_id = ?", ownerID).
		Update("last_heartbeat_at", now.Add(-3*owner.config.InstanceStaleAfter)).Error)
	workflow := &Workflow{
		OwnerInstanceID: ownerID,
		OwnerBootID:     "boot-error",
		LeaseUntil:      pointerTime(now.Add(-time.Second)),
	}

	proven, err := other.confirmForeignRuntimeTermination(context.Background(), workflow, now)
	require.Error(t, err)
	require.False(t, proven)
	var instance Instance
	require.NoError(t, owner.db.Where("instance_id = ?", ownerID).Take(&instance).Error)
	require.NotEqual(t, InstanceStopped, instance.State)
}

func pointerTime(value time.Time) *time.Time { return &value }
