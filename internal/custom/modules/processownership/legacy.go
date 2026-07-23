package processownership

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

var (
	// ErrLegacyTaskRepaired is returned after an unfenced, pre-ownership task
	// has been converted into an explicit terminal row. Returning a non-nil
	// error prevents the first new worker that observes the legacy payload from
	// silently acknowledging work that it could not safely execute.
	ErrLegacyTaskRepaired = errors.New("legacy processing task was terminally repaired")
	ErrLegacyTaskConflict = errors.New("legacy processing repair lost its compare-and-swap")
)

// LegacyProcessingRepairRepository is intentionally narrow: old payloads do
// not contain a generation that can authorize normal writes. The production
// implementation may only consume a row whose persisted generation and owner
// are both empty, and installs a non-empty repair generation atomically with
// the terminal lifecycle transition.
type LegacyProcessingRepairRepository interface {
	RepairLegacyProcessing(
		ctx context.Context,
		tenantID uint64,
		knowledgeID string,
		knowledgeBaseID string,
		expectedStatus string,
		expectedProcessedAt bool,
		repairGeneration string,
		completeCore bool,
		message string,
		updatedAt time.Time,
	) (bool, error)
}

// RepairLegacyTask closes the rolling-upgrade gap for tasks persisted before
// processing_generation/processing_owner existed. A task is safely stale when
// the row is gone, moved, terminal, or already has a generation. Only an
// active row with both ownership fields empty is eligible for repair.
func RepairLegacyTask(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	taskKind string,
) error {
	knowledgeID = strings.TrimSpace(knowledgeID)
	knowledgeBaseID = strings.TrimSpace(knowledgeBaseID)
	taskKind = strings.TrimSpace(taskKind)
	if repo == nil || tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" || taskKind == "" {
		return errors.New("legacy processing repair requires complete task identity")
	}

	knowledge, err := repo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
	if err != nil {
		return fmt.Errorf("legacy %s task: load knowledge: %w", taskKind, err)
	}
	if knowledge == nil {
		return fmt.Errorf("legacy %s task: knowledge row was not returned", taskKind)
	}
	if knowledge.KnowledgeBaseID != knowledgeBaseID {
		return nil
	}
	if strings.TrimSpace(knowledge.ProcessingGeneration) != "" {
		// A newer producer already owns the row. The generation-less task is
		// provably stale and may acknowledge without touching that generation.
		return nil
	}
	if strings.TrimSpace(knowledge.ProcessingOwner) != "" {
		return fmt.Errorf("legacy %s task: row has a partial processing identity", taskKind)
	}
	switch knowledge.ParseStatus {
	case types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing:
	default:
		return nil
	}

	repairer, ok := repo.(LegacyProcessingRepairRepository)
	if !ok || repairer == nil {
		return fmt.Errorf("legacy %s task: terminal repair repository is unavailable", taskKind)
	}
	// processed_at is the durable core-commit boundary. Pending is never
	// treated as committed even if an old row retained a stale timestamp.
	completeCore := knowledge.ParseStatus != types.ParseStatusPending && knowledge.ProcessedAt != nil
	message := fmt.Sprintf(
		"legacy %s task lacked processing ownership after upgrade; reparse is required",
		taskKind,
	)
	repaired, err := repairer.RepairLegacyProcessing(
		ctx,
		tenantID,
		knowledgeID,
		knowledgeBaseID,
		knowledge.ParseStatus,
		knowledge.ProcessedAt != nil,
		uuid.NewString(),
		completeCore,
		message,
		time.Now(),
	)
	if err != nil {
		return fmt.Errorf("legacy %s task: terminal repair: %w", taskKind, err)
	}
	if !repaired {
		return fmt.Errorf("legacy %s task: %w", taskKind, ErrLegacyTaskConflict)
	}
	return fmt.Errorf("legacy %s task: %w", taskKind, ErrLegacyTaskRepaired)
}
