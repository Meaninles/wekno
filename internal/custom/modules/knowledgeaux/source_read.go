package knowledgeaux

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// SourceFileServiceForRead resolves the original file of an active knowledge
// row. New records use their signed ownership binding. Historical records that
// predate the ownership ledger may use the current, path-proven storage profile
// only after the exact tenant/KB/knowledge/file-path tuple is confirmed in the
// knowledge table.
//
// This read-only fallback deliberately does not create ownership and must not
// be used by processing, write, recovery, or deletion paths.
func (r *Registry) SourceFileServiceForRead(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	path string,
	fallbackProvider string,
) (interfaces.FileService, error) {
	knowledgeBaseID = strings.TrimSpace(knowledgeBaseID)
	knowledgeID = strings.TrimSpace(knowledgeID)
	path = strings.TrimSpace(path)
	if r == nil || r.db == nil || tenantID == 0 ||
		knowledgeBaseID == "" || knowledgeID == "" || path == "" {
		return nil, ErrInvalidObject
	}

	service, err := r.FileServiceForPath(
		ctx, tenantID, knowledgeBaseID, knowledgeID, path, fallbackProvider,
	)
	if err == nil {
		return service, nil
	}
	// Existing corrupt, mismatched, or quarantined bindings remain fail-closed.
	// Only an entirely absent legacy reservation is eligible for source reads.
	if !errors.Is(err, ErrReservationLost) {
		return nil, err
	}

	var owner types.Knowledge
	result := r.db.WithContext(ctx).
		Select("id").
		Where(
			"tenant_id = ? AND knowledge_base_id = ? AND id = ? AND file_path = ?",
			tenantID, knowledgeBaseID, knowledgeID, path,
		).
		Take(&owner)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrReservationLost
		}
		return nil, fmt.Errorf("verify legacy source file owner: %w", result.Error)
	}

	tenant, err := r.tenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	provider, err := ProviderForPath(path, fallbackProvider)
	if err != nil {
		return nil, err
	}
	service, err = r.serviceForBackfill(ctx, tenant, provider, path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve legacy source file %q: %v", ErrProviderRouting, path, err)
	}
	return service, nil
}
