package corefanout

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// transactionCompletionStore keeps durable fan-in reads on the transaction
// that owns the KB/knowledge recheck. This is required in Lite mode, whose
// SQLite pool intentionally has one connection; calling the base repository
// from inside the transaction would wait forever for that same connection.
// It also gives PostgreSQL one coherent snapshot for recheck and ledger state.
type transactionCompletionStore struct {
	tx *gorm.DB
}

var _ processownership.DurableFanoutCompletionStore = (*transactionCompletionStore)(nil)

func validateCompletionIdentity(
	tenantID uint64,
	knowledgeID, knowledgeBaseID, generation string,
) error {
	if tenantID == 0 || strings.TrimSpace(knowledgeID) == "" ||
		strings.TrimSpace(knowledgeBaseID) == "" || strings.TrimSpace(generation) == "" {
		return errors.New("core fanout completion store: complete identity is required")
	}
	return nil
}

func (s *transactionCompletionStore) RecordKnowledgeFanoutCompletion(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, knowledgeBaseID, generation, itemID string,
) (bool, error) {
	if s == nil || s.tx == nil {
		return false, errors.New("core fanout completion store: transaction is unavailable")
	}
	if err := validateCompletionIdentity(tenantID, knowledgeID, knowledgeBaseID, generation); err != nil {
		return false, err
	}
	if strings.TrimSpace(itemID) == "" {
		return false, errors.New("core fanout completion store: item identity is required")
	}

	query := s.tx.WithContext(ctx).Table("knowledges").Select("id").Where(
		"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status IN ? AND deleted_at IS NULL",
		tenantID, knowledgeID, knowledgeBaseID, generation,
		[]string{types.ParseStatusPending, types.ParseStatusProcessing},
	)
	if s.tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var owner struct{ ID string }
	if err := query.Take(&owner).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	row := &types.KnowledgeFanoutCompletion{
		TenantID:             tenantID,
		KnowledgeID:          knowledgeID,
		KnowledgeBaseID:      knowledgeBaseID,
		ProcessingGeneration: generation,
		ItemID:               itemID,
		CompletedAt:          time.Now().UTC(),
	}
	result := s.tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	return result.RowsAffected == 1, result.Error
}

func (s *transactionCompletionStore) ListKnowledgeFanoutCompletions(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, knowledgeBaseID, generation string,
) ([]string, error) {
	if s == nil || s.tx == nil {
		return nil, errors.New("core fanout completion store: transaction is unavailable")
	}
	if err := validateCompletionIdentity(tenantID, knowledgeID, knowledgeBaseID, generation); err != nil {
		return nil, err
	}
	var items []string
	err := s.tx.WithContext(ctx).Model(&types.KnowledgeFanoutCompletion{}).
		Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ?",
			tenantID, knowledgeID, knowledgeBaseID, generation,
		).
		Pluck("item_id", &items).Error
	return items, err
}

func (s *transactionCompletionStore) CountKnowledgeFanoutCompletions(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, knowledgeBaseID, generation string,
) (int64, error) {
	if s == nil || s.tx == nil {
		return 0, errors.New("core fanout completion store: transaction is unavailable")
	}
	if err := validateCompletionIdentity(tenantID, knowledgeID, knowledgeBaseID, generation); err != nil {
		return 0, err
	}
	var count int64
	err := s.tx.WithContext(ctx).Model(&types.KnowledgeFanoutCompletion{}).
		Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND item_id NOT LIKE ?",
			tenantID, knowledgeID, knowledgeBaseID, generation, "enrichment:%",
		).
		Count(&count).Error
	return count, err
}

func (s *transactionCompletionStore) KnowledgeFanoutCompletionExists(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, knowledgeBaseID, generation, itemID string,
) (bool, error) {
	if s == nil || s.tx == nil {
		return false, errors.New("core fanout completion store: transaction is unavailable")
	}
	if err := validateCompletionIdentity(tenantID, knowledgeID, knowledgeBaseID, generation); err != nil {
		return false, err
	}
	if strings.TrimSpace(itemID) == "" {
		return false, errors.New("core fanout completion store: item identity is required")
	}
	var count int64
	err := s.tx.WithContext(ctx).Model(&types.KnowledgeFanoutCompletion{}).
		Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND item_id = ?",
			tenantID, knowledgeID, knowledgeBaseID, generation, itemID,
		).
		Limit(1).
		Count(&count).Error
	return count > 0, err
}
