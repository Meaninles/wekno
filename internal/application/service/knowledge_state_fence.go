package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func validateEnrichmentGeneration(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
) (bool, error) {
	if repo == nil || tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" || processingGeneration == "" {
		return false, nil
	}
	knowledge, err := repo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
	if err != nil {
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return false, nil
		}
		return false, err
	}
	return knowledge != nil &&
		knowledge.KnowledgeBaseID == knowledgeBaseID &&
		knowledge.ProcessingGeneration == processingGeneration &&
		knowledge.ParseStatus == types.ParseStatusFinalizing, nil
}

var errKnowledgeStateFenceConflict = errors.New("knowledge state changed during operation")

// cleanupArtifactsBeforeFailureTransition is the ordering barrier for partial
// external writes. Publishing Failed first would make housekeeping/reparse
// race artifacts that are still being removed. If cleanup fails, the lifecycle
// row deliberately remains active so the task can retry safely.
func cleanupArtifactsBeforeFailureTransition(cleanup func() error, transition func() error) error {
	if cleanup == nil || transition == nil {
		return errors.New("cleanup and failure transition are required")
	}
	if err := cleanup(); err != nil {
		return err
	}
	return transition()
}

// knowledgeStateFenceRepository is deliberately narrower than the shared
// KnowledgeRepository interface. Only the production repository and focused
// state-transition tests need to implement this destructive-work fence.
type knowledgeStateFenceRepository interface {
	CompareAndSwapKnowledgeState(
		ctx context.Context,
		tenantID uint64,
		id string,
		expectedKnowledgeBaseID string,
		expectedParseStatus string,
		values map[string]interface{},
	) (bool, error)
}

type knowledgeGenerationFenceRepository interface {
	CompareAndSwapKnowledgeGeneration(
		ctx context.Context,
		tenantID uint64,
		id string,
		expectedKnowledgeBaseID string,
		expectedParseStatus string,
		expectedGeneration string,
		values map[string]interface{},
	) (bool, error)
}

type documentProcessingFenceRepository interface {
	CompareAndSwapDocumentProcessing(
		ctx context.Context,
		tenantID uint64,
		id string,
		expectedKnowledgeBaseID string,
		expectedParseStatus string,
		expectedGeneration string,
		expectedOwner string,
		values map[string]interface{},
	) (bool, error)
}

type processingGenerationFenceRepository interface {
	CompareAndSwapKnowledgeProcessingGeneration(
		ctx context.Context,
		tenantID uint64,
		id string,
		expectedKnowledgeBaseID string,
		expectedGeneration string,
		expectedStatuses []string,
		values map[string]interface{},
	) (bool, error)
}

type knowledgeFanoutCompletionCleaner interface {
	CleanupKnowledgeFanoutCompletions(
		ctx context.Context,
		tenantID uint64,
		knowledgeID string,
		knowledgeBaseID string,
		keepGeneration string,
	) error
}

func cleanupKnowledgeFanoutCompletions(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	keepGeneration string,
) error {
	cleaner, ok := repo.(knowledgeFanoutCompletionCleaner)
	if !ok || cleaner == nil {
		return errors.New("knowledge fanout completion cleaner is unavailable")
	}
	return cleaner.CleanupKnowledgeFanoutCompletions(
		ctx, tenantID, knowledgeID, knowledgeBaseID, keepGeneration,
	)
}

// knowledgeStorageFinalizer is a fail-closed production extension. Keeping it
// narrow avoids forcing read-only repository fakes to implement a mutating
// cross-table transaction while ensuring ingestion never falls back to the
// old split knowledge/storage writes.
type knowledgeStorageFinalizer interface {
	FinalizeKnowledgeWithStorage(
		ctx context.Context,
		knowledge *types.Knowledge,
		expectedParseStatus string,
		storageDelta int64,
	) (bool, error)
	ResetKnowledgeStorage(
		ctx context.Context,
		tenantID uint64,
		knowledgeID string,
		expectedKnowledgeBaseID string,
		expectedParseStatus string,
		expectedGeneration string,
		expectedStorageSize int64,
	) (bool, error)
}

type ownedKnowledgeStorageFinalizer interface {
	FinalizeKnowledgeWithStorageOwned(
		ctx context.Context,
		knowledge *types.Knowledge,
		expectedParseStatus string,
		expectedGeneration string,
		expectedOwner string,
		storageDelta int64,
	) (bool, error)
}

func finalizeKnowledgeWithStorageOwned(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	knowledge *types.Knowledge,
	expectedParseStatus string,
	expectedGeneration string,
	expectedOwner string,
	storageDelta int64,
) (bool, error) {
	finalizer, ok := repo.(ownedKnowledgeStorageFinalizer)
	if !ok || finalizer == nil {
		return false, errors.New("atomic owned knowledge storage finalizer is unavailable")
	}
	return finalizer.FinalizeKnowledgeWithStorageOwned(
		ctx,
		knowledge,
		expectedParseStatus,
		expectedGeneration,
		expectedOwner,
		storageDelta,
	)
}

func compareAndSwapDocumentProcessing(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	expectedOwner string,
	values map[string]interface{},
) (bool, error) {
	fencedRepo, ok := repo.(documentProcessingFenceRepository)
	if !ok || fencedRepo == nil {
		return false, errors.New("document processing fence repository is unavailable")
	}
	return fencedRepo.CompareAndSwapDocumentProcessing(
		ctx,
		tenantID,
		id,
		expectedKnowledgeBaseID,
		expectedParseStatus,
		expectedGeneration,
		expectedOwner,
		values,
	)
}

func compareAndSwapProcessingGeneration(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
	expectedStatuses []string,
	values map[string]interface{},
) (bool, error) {
	fencedRepo, ok := repo.(processingGenerationFenceRepository)
	if !ok || fencedRepo == nil {
		return false, errors.New("processing generation fence repository is unavailable")
	}
	return fencedRepo.CompareAndSwapKnowledgeProcessingGeneration(
		ctx,
		tenantID,
		id,
		expectedKnowledgeBaseID,
		expectedGeneration,
		expectedStatuses,
		values,
	)
}

func (s *knowledgeService) updateCurrentEnrichmentColumns(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
	values map[string]interface{},
	operation string,
) error {
	swapped, err := compareAndSwapProcessingGeneration(
		ctx,
		s.repo,
		tenantID,
		knowledgeID,
		knowledgeBaseID,
		processingGeneration,
		[]string{types.ParseStatusFinalizing},
		values,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if !swapped {
		return fmt.Errorf("%s: %w", operation, errKnowledgeStateFenceConflict)
	}
	return nil
}

func finalizeKnowledgeWithStorage(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	knowledge *types.Knowledge,
	expectedParseStatus string,
	storageDelta int64,
) (bool, error) {
	finalizer, ok := repo.(knowledgeStorageFinalizer)
	if !ok || finalizer == nil {
		return false, errors.New("atomic knowledge storage finalizer is unavailable")
	}
	return finalizer.FinalizeKnowledgeWithStorage(ctx, knowledge, expectedParseStatus, storageDelta)
}

func resetKnowledgeStorage(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	knowledge *types.Knowledge,
) (bool, error) {
	if knowledge == nil {
		return false, errors.New("reset knowledge storage: knowledge is required")
	}
	finalizer, ok := repo.(knowledgeStorageFinalizer)
	if !ok || finalizer == nil {
		return false, errors.New("atomic knowledge storage finalizer is unavailable")
	}
	return finalizer.ResetKnowledgeStorage(
		ctx,
		knowledge.TenantID,
		knowledge.ID,
		knowledge.KnowledgeBaseID,
		knowledge.ParseStatus,
		knowledge.ProcessingGeneration,
		knowledge.StorageSize,
	)
}

func compareAndSwapKnowledgeGeneration(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	values map[string]interface{},
) (bool, error) {
	fencedRepo, ok := repo.(knowledgeGenerationFenceRepository)
	if !ok || fencedRepo == nil {
		return false, errors.New("knowledge generation fence repository is unavailable")
	}
	return fencedRepo.CompareAndSwapKnowledgeGeneration(
		ctx,
		tenantID,
		id,
		expectedKnowledgeBaseID,
		expectedParseStatus,
		expectedGeneration,
		values,
	)
}

func compareAndSwapKnowledgeState(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	values map[string]interface{},
) (bool, error) {
	fencedRepo, ok := repo.(knowledgeStateFenceRepository)
	if !ok || fencedRepo == nil {
		return false, errors.New("knowledge state fence repository is unavailable")
	}
	return fencedRepo.CompareAndSwapKnowledgeState(
		ctx,
		tenantID,
		id,
		expectedKnowledgeBaseID,
		expectedParseStatus,
		values,
	)
}

func (s *knowledgeService) requireKnowledgeStateSwap(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	values map[string]interface{},
	operation string,
) error {
	swapped, err := compareAndSwapKnowledgeState(
		ctx,
		s.repo,
		tenantID,
		id,
		expectedKnowledgeBaseID,
		expectedParseStatus,
		values,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if !swapped {
		return fmt.Errorf("%s: %w", operation, errKnowledgeStateFenceConflict)
	}
	return nil
}

func (s *knowledgeService) requireKnowledgeGenerationSwap(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	values map[string]interface{},
	operation string,
) error {
	swapped, err := compareAndSwapKnowledgeGeneration(
		ctx,
		s.repo,
		tenantID,
		id,
		expectedKnowledgeBaseID,
		expectedParseStatus,
		expectedGeneration,
		values,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if !swapped {
		return fmt.Errorf("%s: %w", operation, errKnowledgeStateFenceConflict)
	}
	return nil
}

func (s *knowledgeService) requireDocumentProcessingSwap(
	ctx context.Context,
	knowledge *types.Knowledge,
	expectedParseStatus string,
	expectedOwner string,
	values map[string]interface{},
	operation string,
) error {
	if knowledge == nil {
		return fmt.Errorf("%s: knowledge is required", operation)
	}
	swapped, err := compareAndSwapDocumentProcessing(
		ctx,
		s.repo,
		knowledge.TenantID,
		knowledge.ID,
		knowledge.KnowledgeBaseID,
		expectedParseStatus,
		knowledge.ProcessingGeneration,
		expectedOwner,
		values,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if !swapped {
		return fmt.Errorf("%s: %w", operation, errKnowledgeStateFenceConflict)
	}
	return nil
}

func (s *knowledgeService) requireDocumentProcessingIdentitySwap(
	ctx context.Context,
	knowledge *types.Knowledge,
	expectedParseStatus string,
	expectedGeneration string,
	expectedOwner string,
	values map[string]interface{},
	operation string,
) error {
	if knowledge == nil {
		return fmt.Errorf("%s: knowledge is required", operation)
	}
	swapped, err := compareAndSwapDocumentProcessing(
		ctx,
		s.repo,
		knowledge.TenantID,
		knowledge.ID,
		knowledge.KnowledgeBaseID,
		expectedParseStatus,
		expectedGeneration,
		expectedOwner,
		values,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if !swapped {
		return fmt.Errorf("%s: %w", operation, errKnowledgeStateFenceConflict)
	}
	return nil
}

func (s *knowledgeService) updateActiveProcessingColumns(
	ctx context.Context,
	knowledge *types.Knowledge,
	values map[string]interface{},
	operation string,
) error {
	if knowledge == nil {
		return fmt.Errorf("%s: knowledge is required", operation)
	}
	if knowledge.ProcessingGeneration == "" || knowledge.ProcessingOwner == "" {
		return fmt.Errorf("%s: processing generation and owner are required", operation)
	}
	return s.requireDocumentProcessingIdentitySwap(
		ctx,
		knowledge,
		types.ParseStatusProcessing,
		knowledge.ProcessingGeneration,
		knowledge.ProcessingOwner,
		values,
		operation,
	)
}

func (s *knowledgeService) markActiveProcessingFailed(
	ctx context.Context,
	knowledge *types.Knowledge,
	message string,
	operation string,
) error {
	now := time.Now()
	values := processingFailureValuesPreservingStorage(message, now)
	values["processing_owner"] = ""
	if err := s.updateActiveProcessingColumns(ctx, knowledge, values, operation); err != nil {
		return err
	}
	knowledge.ParseStatus = types.ParseStatusFailed
	knowledge.ErrorMessage = message
	knowledge.PendingSubtasksCount = 0
	knowledge.ProcessingOwner = ""
	knowledge.UpdatedAt = now
	return nil
}

// failPendingDocumentDispatch closes the gap between persisting a pending
// document row and successfully publishing its only core-processing task. A
// dispatch failure must be visible immediately to the caller and must consume
// only the exact generation/owner that this producer allocated.
func (s *knowledgeService) failPendingDocumentDispatch(
	ctx context.Context,
	knowledge *types.Knowledge,
	cause error,
	operation string,
) error {
	if cause == nil {
		cause = errors.New("document processing task dispatch failed")
	}
	if knowledge == nil {
		return cause
	}
	now := time.Now()
	values := processingFailureValuesPreservingStorage(cause.Error(), now)
	values["processing_owner"] = ""
	values["processing_fanout"] = nil
	if err := s.requireDocumentProcessingIdentitySwap(
		ctx,
		knowledge,
		types.ParseStatusPending,
		knowledge.ProcessingGeneration,
		knowledge.ProcessingOwner,
		values,
		operation,
	); err != nil {
		return fmt.Errorf("%w; additionally failed to close pending generation: %v", cause, err)
	}
	knowledge.ParseStatus = types.ParseStatusFailed
	knowledge.ErrorMessage = cause.Error()
	knowledge.PendingSubtasksCount = 0
	knowledge.ProcessingOwner = ""
	knowledge.ProcessingFanout = nil
	knowledge.UpdatedAt = now
	return cause
}

func (s *knowledgeService) heartbeatActiveProcessing(
	ctx context.Context,
	knowledge *types.Knowledge,
	operation string,
) error {
	now := time.Now()
	if err := s.updateActiveProcessingColumns(ctx, knowledge, map[string]interface{}{
		"updated_at": now,
	}, operation); err != nil {
		return err
	}
	knowledge.UpdatedAt = now
	return nil
}
