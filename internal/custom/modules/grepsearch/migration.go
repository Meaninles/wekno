package grepsearch

import (
	"context"
	"fmt"
	"strings"

	schema "github.com/Tencent/WeKnora/migrations/custom/grepsearch"
	"gorm.io/gorm"
)

// Migrate is called only by the one-shot migration role. Read replicas never
// construct or lazily repair a projection while serving requests.
func Migrate(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("grep search: database unavailable")
	}
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(734819026)").Error; err != nil {
			return err
		}
		if tx.Migrator().HasTable("custom_grepsearch_state") {
			return Validate(ctx, tx)
		}
		var name string
		if err := tx.Raw("SELECT current_schema()").Scan(&name).Error; err != nil {
			return err
		}
		if name == "" {
			return fmt.Errorf("grep search: database schema unavailable")
		}
		// Capture this exact schema in trigger functions, not a caller's later search_path.
		if err := tx.Exec(`SET LOCAL search_path = "` + strings.ReplaceAll(name, `"`, `""`) + `", pg_catalog`).Error; err != nil {
			return err
		}
		if err := tx.Exec(schema.Up).Error; err != nil {
			return fmt.Errorf("install grep projection: %w", err)
		}
		return Validate(ctx, tx)
	})
}

func Validate(ctx context.Context, db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	var ready bool
	err := db.WithContext(ctx).Raw(`SELECT
        (SELECT version=1 FROM custom_grepsearch_state WHERE id=1)
        AND to_regclass('custom_grepsearch_chunks') IS NOT NULL
        AND (SELECT count(*)=8 FROM pg_trigger WHERE
            tgrelid IN ('chunks'::regclass,'knowledges'::regclass)
            AND tgname LIKE 'custom_grepsearch_%' AND tgenabled IN ('O','A'))`).Scan(&ready).Error
	if err != nil {
		return fmt.Errorf("grep projection is not installed; run migration role: %w", err)
	}
	if !ready {
		return fmt.Errorf("grep projection version/triggers are not ready; run migration role")
	}
	return nil
}
