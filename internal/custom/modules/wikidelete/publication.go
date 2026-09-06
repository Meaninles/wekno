package wikidelete

import (
	"bytes"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// QuarantineSourceTx hides materialized claims in the same transaction that
// retires their source. The existing retract worker retains source refs and
// can later rebuild shared pages; callers cannot observe retired facts meanwhile.
func QuarantineSourceTx(tx *gorm.DB, tenantID uint64, kbID, sourceID string) error {
	query, err := SourceRefQuery(tx.Model(&types.WikiPage{}), kbID, sourceID)
	if err != nil {
		return err
	}
	var ids []string
	if err := query.Where("tenant_id = ?", tenantID).Pluck("id", &ids).Error; err != nil {
		return err
	}
	var pages []*types.WikiPage
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND knowledge_base_id = ? AND (id IN ? OR page_type IN ?)", tenantID, kbID, ids, []string{types.WikiPageTypeIndex, types.WikiPageTypeLog}).Order("id").Find(&pages).Error; err != nil {
		return err
	}
	for _, page := range pages {
		beforeStatus := page.Status
		beforeMetadata := append([]byte(nil), page.PageMetadata...)
		if err := Quarantine(page, sourceID); err != nil {
			return err
		}
		if beforeStatus == page.Status && bytes.Equal(beforeMetadata, page.PageMetadata) {
			continue
		}
		if err := tx.Model(&types.WikiPage{}).Where("id = ? AND tenant_id = ?", page.ID, tenantID).Updates(map[string]any{"status": page.Status, "page_metadata": page.PageMetadata, "version": gorm.Expr("version + 1"), "updated_at": time.Now()}).Error; err != nil {
			return err
		}
	}
	return nil
}
