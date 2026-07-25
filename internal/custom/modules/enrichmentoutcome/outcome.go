package enrichmentoutcome

import (
	"strings"
	"time"
)

const (
	StatusCompleted = "completed"
	StatusDegraded  = "degraded"
	StatusFailed    = "failed"

	MaxDetailRunes = 4096
)

type Outcome struct {
	TenantID             uint64    `gorm:"primaryKey;column:tenant_id"`
	KnowledgeID          string    `gorm:"primaryKey;column:knowledge_id"`
	KnowledgeBaseID      string    `gorm:"column:knowledge_base_id;not null"`
	ProcessingGeneration string    `gorm:"primaryKey;column:processing_generation"`
	ItemID               string    `gorm:"primaryKey;column:item_id"`
	Status               string    `gorm:"column:status;not null"`
	Detail               string    `gorm:"column:detail;not null;default:''"`
	CompletedAt          time.Time `gorm:"column:completed_at;not null"`
}

func (Outcome) TableName() string {
	return "custom_enrichment_outcomes"
}

func ValidStatus(status string) bool {
	switch status {
	case StatusCompleted, StatusDegraded, StatusFailed:
		return true
	default:
		return false
	}
}

func NormalizeDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	runes := []rune(detail)
	if len(runes) <= MaxDetailRunes {
		return detail
	}
	return string(runes[:MaxDetailRunes])
}
