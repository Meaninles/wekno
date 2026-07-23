package documentqueue

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

func seedExpiredRecoveryWorkflow(
	t *testing.T,
	coordinator *Coordinator,
	tenantID uint64,
	knowledgeID string,
	generation string,
	ownerInstanceID string,
	ownerBootID string,
	leaseUntil time.Time,
) *Workflow {
	return seedExpiredRecoveryWorkflowWithID(
		t,
		coordinator,
		tenantID,
		knowledgeID,
		generation,
		ownerInstanceID,
		ownerBootID,
		leaseUntil,
		"",
	)
}

func seedExpiredRecoveryWorkflowWithID(
	t *testing.T,
	coordinator *Coordinator,
	tenantID uint64,
	knowledgeID string,
	generation string,
	ownerInstanceID string,
	ownerBootID string,
	leaseUntil time.Time,
	forcedID string,
) *Workflow {
	t.Helper()
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(),
		types.TypeDocumentProcess,
		workflowPayload(t, tenantID, knowledgeID, generation),
	)
	require.NoError(t, err)
	if forcedID != "" {
		result := coordinator.db.Model(&Workflow{}).
			Where("id = ?", workflow.ID).
			UpdateColumn("id", forcedID)
		require.NoError(t, result.Error)
		require.EqualValues(t, 1, result.RowsAffected)
		workflow.ID = forcedID
	}
	bindWorkflowForTest(t, coordinator, workflow)
	require.NoError(t, coordinator.db.Model(&Workflow{}).
		Where("id = ?", workflow.ID).
		Updates(map[string]interface{}{
			"state":             StateLeased,
			"stage":             "core",
			"owner_instance_id": ownerInstanceID,
			"owner_boot_id":     ownerBootID,
			"lease_until":       leaseUntil,
		}).Error)
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(workflow).Error)
	return workflow
}

func TestExpiredRecoveryFixedSweepRevisitsOldRowsDespiteGrowingTail(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "recovery-growing-tail", "growing-tail-boot", 1)
	coordinator.config.RecoveryBatchSize = 1
	now := time.Now()
	staleHeartbeat := now.Add(-4 * coordinator.config.InstanceStaleAfter)
	ownerID := "k8s/recovery-ns/growing-tail-owner"
	ownerBootID := "growing-tail-owner-boot"
	require.NoError(t, coordinator.db.Create(&Instance{
		InstanceID:      ownerID,
		BootID:          ownerBootID,
		State:           InstanceReady,
		Capacity:        expiredRecoveryScanMultiplier + 1,
		StartedAt:       staleHeartbeat,
		LastHeartbeatAt: staleHeartbeat,
	}).Error)
	verifier := &fakeRuntimeTerminationVerifier{
		evidence: RuntimeTerminationEvidence{Reason: "pod_uid_not_present_is_not_proof"},
	}
	coordinator.runtimeVerifier = verifier

	oldestLease := now.Add(-4 * time.Hour)
	old := seedExpiredRecoveryWorkflow(
		t,
		coordinator,
		2100,
		"growing-tail-old-knowledge",
		"growing-tail-old-generation",
		ownerID,
		ownerBootID,
		oldestLease,
	)
	for i := 0; i < expiredRecoveryScanMultiplier; i++ {
		seedExpiredRecoveryWorkflow(
			t,
			coordinator,
			uint64(2101+i),
			fmt.Sprintf("growing-tail-filler-knowledge-%02d", i),
			fmt.Sprintf("growing-tail-filler-generation-%02d", i),
			ownerID,
			ownerBootID,
			oldestLease.Add(time.Duration(i+1)*time.Second),
		)
	}

	// The oldest row is intentionally fail-closed. The first cycle consumes
	// its entire scan budget before reaching the frozen high-water row.
	require.NoError(t, coordinator.recoverExpiredLeases(context.Background(), now))
	require.EqualValues(t, 1, verifier.calls.Load())
	require.True(t, coordinator.expiredRecoveryCursor.Valid)
	require.True(t, coordinator.expiredRecoveryCursor.PositionValid)
	var current Workflow
	require.NoError(t, coordinator.db.Where("id = ?", old.ID).Take(&current).Error)
	require.Equal(t, StateLeased, current.State)

	// Termination becomes provable after that first pass. Before every later
	// cycle, append at least a full scan budget beyond the original high-water
	// mark. A dynamic-tail cursor would chase these rows forever.
	require.NoError(t, coordinator.db.Model(&Instance{}).
		Where("instance_id = ? AND boot_id = ?", ownerID, ownerBootID).
		Updates(map[string]interface{}{
			"state":      InstanceStopped,
			"stopped_at": now,
		}).Error)
	appendTail := func(round int, base time.Time) {
		t.Helper()
		for i := 0; i < expiredRecoveryScanMultiplier; i++ {
			sequence := round*expiredRecoveryScanMultiplier + i
			seedExpiredRecoveryWorkflow(
				t,
				coordinator,
				uint64(2200+sequence),
				fmt.Sprintf("growing-tail-new-knowledge-%03d", sequence),
				fmt.Sprintf("growing-tail-new-generation-%03d", sequence),
				ownerID,
				ownerBootID,
				base.Add(time.Duration(i)*time.Second),
			)
		}
	}

	appendTail(0, now.Add(-2*time.Hour))
	require.NoError(t, coordinator.recoverExpiredLeases(context.Background(), now))
	require.False(t, coordinator.expiredRecoveryCursor.Valid,
		"the original frozen sweep must finish instead of extending into its new tail")
	require.NoError(t, coordinator.db.Where("id = ?", old.ID).Take(&current).Error)
	require.Equal(t, StateLeased, current.State)

	appendTail(1, now.Add(-time.Hour))
	require.NoError(t, coordinator.recoverExpiredLeases(context.Background(), now))
	require.NoError(t, coordinator.db.Where("id = ?", old.ID).Take(&current).Error)
	require.Equal(t, StateQueued, current.State,
		"the completed sweep must wrap and revisit the old row in finite cycles")
	require.EqualValues(t, old.DispatchEpoch+1, current.DispatchEpoch)
}

func TestExpiredRecoveryFixedSweepUsesIDAtEqualLeaseBoundary(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "recovery-equal-lease", "equal-lease-boot", 1)
	coordinator.config.RecoveryBatchSize = 1
	now := time.Now()
	staleHeartbeat := now.Add(-4 * coordinator.config.InstanceStaleAfter)
	blockedOwnerID := "k8s/recovery-ns/equal-lease-blocked"
	blockedBootID := "equal-lease-blocked-boot"
	require.NoError(t, coordinator.db.Create(&Instance{
		InstanceID:      blockedOwnerID,
		BootID:          blockedBootID,
		State:           InstanceReady,
		Capacity:        expiredRecoveryScanMultiplier + 1,
		StartedAt:       staleHeartbeat,
		LastHeartbeatAt: staleHeartbeat,
	}).Error)
	tailOwnerID := "equal-lease-terminated"
	tailBootID := "equal-lease-terminated-boot"
	require.NoError(t, coordinator.db.Create(&Instance{
		InstanceID:      tailOwnerID,
		BootID:          tailBootID,
		State:           InstanceStopped,
		Capacity:        1,
		StartedAt:       staleHeartbeat,
		LastHeartbeatAt: staleHeartbeat,
		StoppedAt:       &staleHeartbeat,
	}).Error)
	verifier := &fakeRuntimeTerminationVerifier{
		evidence: RuntimeTerminationEvidence{Reason: "pod_uid_not_present_is_not_proof"},
	}
	coordinator.runtimeVerifier = verifier

	sharedLease := now.Add(-time.Hour)
	for i := 0; i <= expiredRecoveryScanMultiplier; i++ {
		seedExpiredRecoveryWorkflowWithID(
			t,
			coordinator,
			uint64(2300+i),
			fmt.Sprintf("equal-lease-knowledge-%02d", i),
			fmt.Sprintf("equal-lease-generation-%02d", i),
			blockedOwnerID,
			blockedBootID,
			sharedLease,
			fmt.Sprintf("equal-lease-workflow-%03d", i),
		)
	}

	require.NoError(t, coordinator.recoverExpiredLeases(context.Background(), now))
	require.True(t, coordinator.expiredRecoveryCursor.Valid)
	require.Equal(t, "equal-lease-workflow-010", coordinator.expiredRecoveryCursor.HighWaterWorkflowID)
	require.Equal(t, "equal-lease-workflow-009", coordinator.expiredRecoveryCursor.WorkflowID)

	// This row has the same timestamp but sorts after the frozen ID boundary.
	// It must remain outside the current sweep even though it is reclaimable.
	tail := seedExpiredRecoveryWorkflowWithID(
		t,
		coordinator,
		2400,
		"equal-lease-tail-knowledge",
		"equal-lease-tail-generation",
		tailOwnerID,
		tailBootID,
		sharedLease,
		"equal-lease-workflow-999",
	)
	require.NoError(t, coordinator.recoverExpiredLeases(context.Background(), now))
	require.False(t, coordinator.expiredRecoveryCursor.Valid)
	var current Workflow
	require.NoError(t, coordinator.db.Where("id = ?", tail.ID).Take(&current).Error)
	require.Equal(t, StateLeased, current.State,
		"same-timestamp rows above the frozen ID boundary belong to the next sweep")
}

func TestRecoverExpiredLeasesCursorPassesMoreThanScanBudgetOfUnprovenWork(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "recovery-survivor", "survivor-boot", 1)
	coordinator.config.RecoveryBatchSize = 1
	now := time.Now()
	staleHeartbeat := now.Add(-4 * coordinator.config.InstanceStaleAfter)

	blockedOwnerID := "k8s/recovery-ns/blocked-pod-uid"
	blockedBootID := "blocked-boot"
	require.NoError(t, coordinator.db.Create(&Instance{
		InstanceID:      blockedOwnerID,
		BootID:          blockedBootID,
		State:           InstanceReady,
		Capacity:        expiredRecoveryScanMultiplier + 1,
		StartedAt:       staleHeartbeat,
		LastHeartbeatAt: staleHeartbeat,
	}).Error)
	recoverableOwnerID := "terminated-owner"
	recoverableBootID := "terminated-boot"
	require.NoError(t, coordinator.db.Create(&Instance{
		InstanceID:      recoverableOwnerID,
		BootID:          recoverableBootID,
		State:           InstanceStopped,
		Capacity:        1,
		StartedAt:       staleHeartbeat,
		LastHeartbeatAt: staleHeartbeat,
		StoppedAt:       &staleHeartbeat,
	}).Error)

	verifier := &fakeRuntimeTerminationVerifier{
		evidence: RuntimeTerminationEvidence{Reason: "pod_uid_not_present_is_not_proof"},
	}
	coordinator.runtimeVerifier = verifier
	oldestLease := now.Add(-3 * time.Hour)
	blocked := make([]*Workflow, 0, expiredRecoveryScanMultiplier+1)
	for i := 0; i < expiredRecoveryScanMultiplier+1; i++ {
		blocked = append(blocked, seedExpiredRecoveryWorkflow(
			t,
			coordinator,
			uint64(1800+i),
			fmt.Sprintf("blocked-knowledge-%02d", i),
			fmt.Sprintf("blocked-generation-%02d", i),
			blockedOwnerID,
			blockedBootID,
			oldestLease.Add(time.Duration(i)*time.Second),
		))
	}
	recoverable := seedExpiredRecoveryWorkflow(
		t,
		coordinator,
		1900,
		"recoverable-knowledge",
		"recoverable-generation",
		recoverableOwnerID,
		recoverableBootID,
		oldestLease.Add(time.Duration(expiredRecoveryScanMultiplier+1)*time.Second),
	)

	// The first cycle exhausts its bounded scan budget entirely on safe,
	// fail-closed rows. All of those rows share one runtime lookup.
	require.NoError(t, coordinator.recoverExpiredLeases(context.Background(), now))
	require.EqualValues(t, 1, verifier.calls.Load())
	var current Workflow
	require.NoError(t, coordinator.db.Where("id = ?", recoverable.ID).Take(&current).Error)
	require.Equal(t, StateLeased, current.State)
	require.True(t, coordinator.expiredRecoveryCursor.Valid)

	// The keyset cursor continues after that budget on the next cycle, passes
	// the remaining unproven row, and reaches the later eligible owner.
	require.NoError(t, coordinator.recoverExpiredLeases(context.Background(), now))
	require.EqualValues(t, 2, verifier.calls.Load())
	require.NoError(t, coordinator.db.Where("id = ?", recoverable.ID).Take(&current).Error)
	require.Equal(t, StateQueued, current.State)
	require.EqualValues(t, recoverable.DispatchEpoch+1, current.DispatchEpoch)

	for _, workflow := range blocked {
		current = Workflow{}
		require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&current).Error)
		require.Equal(t, StateLeased, current.State)
	}
}

func TestConcurrentExpiredRecoveryRequeuesEligibleWorkflowOnlyOnce(t *testing.T) {
	first := newQueueTestCoordinator(t, "recovery-racer-a", "racer-boot-a", 1)
	first.config.RecoveryBatchSize = 1
	second := NewCoordinatorWithConfig(
		first.db,
		nil,
		"recovery-racer-b",
		"racer-boot-b",
		1,
		first.config,
	)
	require.NoError(t, second.registerAndAdopt(context.Background()))
	require.NoError(t, second.MarkReady(context.Background()))

	now := time.Now()
	staleHeartbeat := now.Add(-4 * first.config.InstanceStaleAfter)
	require.NoError(t, first.db.Create(&Instance{
		InstanceID:      "concurrently-terminated-owner",
		BootID:          "concurrently-terminated-boot",
		State:           InstanceStopped,
		Capacity:        1,
		StartedAt:       staleHeartbeat,
		LastHeartbeatAt: staleHeartbeat,
		StoppedAt:       &staleHeartbeat,
	}).Error)
	workflow := seedExpiredRecoveryWorkflow(
		t,
		first,
		2001,
		"concurrent-recovery-knowledge",
		"concurrent-recovery-generation",
		"concurrently-terminated-owner",
		"concurrently-terminated-boot",
		now.Add(-time.Hour),
	)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, coordinator := range []*Coordinator{first, second} {
		wait.Add(1)
		go func(candidate *Coordinator) {
			defer wait.Done()
			<-start
			errs <- candidate.recoverExpiredLeases(context.Background(), now)
		}(coordinator)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	var current Workflow
	require.NoError(t, first.db.Where("id = ?", workflow.ID).Take(&current).Error)
	require.Equal(t, StateQueued, current.State)
	require.EqualValues(t, workflow.DispatchEpoch+1, current.DispatchEpoch)
}
