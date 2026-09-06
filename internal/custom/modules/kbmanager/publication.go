package kbmanager

import (
	"context"
	"errors"
	"fmt"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrPublicationConflict = errors.New("replacement publication identity changed")

func (s *Service) publishReplacement(ctx context.Context, op *Operation) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := kbwritefence.LockActive(tx, op.SourceTenantID, op.KnowledgeBaseID); err != nil {
			return err
		}
		var rows []types.Knowledge
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND knowledge_base_id = ? AND id IN ?", op.SourceTenantID, op.KnowledgeBaseID, []string{op.OldKnowledgeID, op.NewKnowledgeID}).Order("id").Find(&rows).Error; err != nil {
			return err
		}
		var old, next *types.Knowledge
		for i := range rows {
			if rows[i].ID == op.OldKnowledgeID {
				old = &rows[i]
			}
			if rows[i].ID == op.NewKnowledgeID {
				next = &rows[i]
			}
		}
		if old == nil || next == nil {
			return fmt.Errorf("%w: replacement identities are unavailable", ErrPublicationConflict)
		}
		if old.FileHash != op.OldFileHash || old.ParseStatus == types.ParseStatusDeleting || old.PublicationState != "published" {
			return fmt.Errorf("%w: old document is no longer the published version", ErrPublicationConflict)
		}
		if next.CoreStatus != types.CoreStatusReady || next.ProcessingGeneration != op.NewGeneration || next.PublicationState != "staged" {
			return fmt.Errorf("%w: replacement generation is not ready for publication", ErrPublicationConflict)
		}
		if err := wikidelete.QuarantineSourceTx(tx, op.SourceTenantID, op.KnowledgeBaseID, old.ID); err != nil {
			return err
		}
		if err := tx.Model(&types.Knowledge{}).Where("id = ?", old.ID).Update("publication_state", "retired").Error; err != nil {
			return err
		}
		if err := tx.Model(&types.Knowledge{}).Where("id = ?", next.ID).Update("publication_state", "published").Error; err != nil {
			return err
		}
		result := tx.Model(&Operation{}).Where("id = ? AND lease_owner = ? AND state = ?", op.ID, op.LeaseOwner, OperationStateParsing).
			Updates(map[string]any{"state": OperationStateCleanup, "searchable": true, "new_generation": op.NewGeneration, "result_message": "新版本已发布，正在清理旧版本"})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("replacement operation lease changed")
		}
		return nil
	})
}
