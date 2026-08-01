package wikiqueue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

type mapDispatchSchema struct {
	NextAttemptAt         *time.Time `gorm:"column:next_attempt_at"`
	MapResourcePoolID     string     `gorm:"column:map_resource_pool_id;type:varchar(64);not null;default:''"`
	MapDispatchEpoch      uint64     `gorm:"column:map_dispatch_epoch;not null;default:0"`
	MapDispatchTaskID     string     `gorm:"column:map_dispatch_task_id;type:varchar(190);not null;default:''"`
	MapDispatchLeaseUntil *time.Time `gorm:"column:map_dispatch_lease_until"`
}

func (mapDispatchSchema) TableName() string { return "task_pending_ops" }

// Migrate installs the PostgreSQL-authoritative Wiki Map outbox columns. It
// runs only on the maintenance profile through custom bootstrap, before any
// task consumer starts.
func Migrate(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("wiki queue migration: database is nil")
	}
	db = db.WithContext(ctx)
	if !db.Migrator().HasTable("task_pending_ops") {
		return errors.New("wiki queue migration: task_pending_ops is missing")
	}
	if db.Dialector.Name() != "postgres" {
		for _, field := range []string{
			"NextAttemptAt",
			"MapResourcePoolID",
			"MapDispatchEpoch",
			"MapDispatchTaskID",
			"MapDispatchLeaseUntil",
		} {
			if db.Migrator().HasColumn(&mapDispatchSchema{}, field) {
				continue
			}
			if err := db.Migrator().AddColumn(&mapDispatchSchema{}, field); err != nil {
				return fmt.Errorf("wiki queue migration: add %s: %w", field, err)
			}
		}
		return db.Exec(
			`UPDATE task_pending_ops
			 SET next_attempt_at = COALESCE(claimed_at, enqueued_at, CURRENT_TIMESTAMP)
			 WHERE task_type = 'wiki:ingest'
			   AND scope = 'knowledge_base'
			   AND op = 'ingest'
			   AND next_attempt_at IS NULL`,
		).Error
	}

	return db.Transaction(func(tx *gorm.DB) error {
		statements := []string{
			`ALTER TABLE task_pending_ops
			 ADD COLUMN IF NOT EXISTS next_attempt_at timestamptz,
			 ADD COLUMN IF NOT EXISTS map_resource_pool_id varchar(64) NOT NULL DEFAULT '',
			 ADD COLUMN IF NOT EXISTS map_dispatch_epoch bigint NOT NULL DEFAULT 0,
			 ADD COLUMN IF NOT EXISTS map_dispatch_task_id varchar(190) NOT NULL DEFAULT '',
			 ADD COLUMN IF NOT EXISTS map_dispatch_lease_until timestamptz`,
			`UPDATE task_pending_ops
			 SET next_attempt_at = COALESCE(claimed_at, enqueued_at, now())
			 WHERE task_type = 'wiki:ingest'
			   AND scope = 'knowledge_base'
			   AND op = 'ingest'
			   AND next_attempt_at IS NULL`,
			`CREATE INDEX IF NOT EXISTS idx_task_pending_ops_wiki_map_dispatch_due
			 ON task_pending_ops (next_attempt_at, map_dispatch_lease_until, id)
			 WHERE task_type = 'wiki:ingest'
			   AND scope = 'knowledge_base'
			   AND op = 'ingest'
			   AND map_ready_at IS NULL`,
			`CREATE INDEX IF NOT EXISTS idx_task_pending_ops_wiki_map_dispatch_pool
			 ON task_pending_ops (map_resource_pool_id, map_dispatch_lease_until)
			 WHERE task_type = 'wiki:ingest'
			   AND scope = 'knowledge_base'
			   AND op = 'ingest'
			   AND map_ready_at IS NULL`,
		}
		for _, statement := range statements {
			if err := tx.Exec(statement).Error; err != nil {
				return fmt.Errorf("wiki queue migration: %w", err)
			}
		}
		return nil
	})
}
