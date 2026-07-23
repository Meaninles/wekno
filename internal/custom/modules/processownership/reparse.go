package processownership

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
)

// BatchReparseIdentity deterministically names the one document generation
// owned by a batch child. The same parent retry produces the same identity;
// another batch receives a different generation and therefore cannot be
// adopted by an older child retry.
func BatchReparseIdentity(tenantID uint64, batchID, knowledgeID string) (generation, owner string) {
	batchID = strings.TrimSpace(batchID)
	knowledgeID = strings.TrimSpace(knowledgeID)
	if tenantID == 0 || batchID == "" || knowledgeID == "" {
		return "", ""
	}
	name := fmt.Sprintf("batch-reparse\x00%d\x00%s\x00%s", tenantID, batchID, knowledgeID)
	generation = uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
	return generation, DocumentOwner(knowledgeID, generation)
}

func CaptureBatchReparseSnapshot(knowledge *types.Knowledge) (types.KnowledgeReparseExpectedSnapshot, error) {
	if knowledge == nil {
		return types.KnowledgeReparseExpectedSnapshot{}, errors.New("batch reparse snapshot: knowledge is required")
	}
	snapshot := types.KnowledgeReparseExpectedSnapshot{
		TenantID:             knowledge.TenantID,
		KnowledgeID:          strings.TrimSpace(knowledge.ID),
		KnowledgeBaseID:      strings.TrimSpace(knowledge.KnowledgeBaseID),
		ParseStatus:          strings.TrimSpace(knowledge.ParseStatus),
		ProcessingGeneration: knowledge.ProcessingGeneration,
		ProcessingOwner:      knowledge.ProcessingOwner,
		UpdatedAt:            knowledge.UpdatedAt,
	}
	if err := ValidateBatchReparseSnapshot(snapshot); err != nil {
		return types.KnowledgeReparseExpectedSnapshot{}, err
	}
	return snapshot, nil
}

func ValidateBatchReparseSnapshot(snapshot types.KnowledgeReparseExpectedSnapshot) error {
	if snapshot.TenantID == 0 || strings.TrimSpace(snapshot.KnowledgeID) == "" ||
		strings.TrimSpace(snapshot.KnowledgeBaseID) == "" || strings.TrimSpace(snapshot.ParseStatus) == "" ||
		snapshot.UpdatedAt.IsZero() {
		return errors.New("batch reparse snapshot: tenant, knowledge, knowledge base, status, and updated_at are required")
	}
	return nil
}

func BatchReparseSnapshotMatches(
	knowledge *types.Knowledge,
	snapshot types.KnowledgeReparseExpectedSnapshot,
) bool {
	if knowledge == nil || ValidateBatchReparseSnapshot(snapshot) != nil {
		return false
	}
	return knowledge.TenantID == snapshot.TenantID &&
		knowledge.ID == snapshot.KnowledgeID &&
		knowledge.KnowledgeBaseID == snapshot.KnowledgeBaseID &&
		knowledge.ParseStatus == snapshot.ParseStatus &&
		knowledge.ProcessingGeneration == snapshot.ProcessingGeneration &&
		knowledge.ProcessingOwner == snapshot.ProcessingOwner &&
		knowledge.UpdatedAt.Equal(snapshot.UpdatedAt)
}
