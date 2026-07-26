package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/storagemigration"
	"github.com/Tencent/WeKnora/internal/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const storageMigrationApplyConfirmation = "--confirm-all-app-replicas-stopped"

func runMaintenanceCommand(ctx context.Context, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "storage-migrate" {
		return false, nil
	}
	if len(args) < 2 {
		return true, errors.New(
			"usage: WeKnora storage-migrate audit | " +
				"WeKnora storage-migrate apply " + storageMigrationApplyConfirmation,
		)
	}
	mode := storagemigration.Mode(strings.ToLower(strings.TrimSpace(args[1])))
	if mode == storagemigration.ModeApply {
		confirmed := false
		for _, arg := range args[2:] {
			if arg == storageMigrationApplyConfirmation {
				confirmed = true
			}
		}
		if !confirmed {
			return true, fmt.Errorf(
				"apply requires %s after every app replica has been stopped",
				storageMigrationApplyConfirmation,
			)
		}
	} else if mode != storagemigration.ModeAudit {
		return true, fmt.Errorf("unsupported storage migration mode %q", mode)
	}

	dsn, err := database.PostgresGormDSNFromEnv()
	if err != nil {
		return true, err
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		NowFunc: func() time.Time { return time.Now().UTC() },
		Logger:  logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return true, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return true, err
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(1)

	concurrency := 2
	if raw := strings.TrimSpace(os.Getenv("CUSTOM_STORAGE_MIGRATION_CONCURRENCY")); raw != "" {
		value, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return true, fmt.Errorf("invalid CUSTOM_STORAGE_MIGRATION_CONCURRENCY: %w", parseErr)
		}
		concurrency = value
	}
	migrator, err := storagemigration.NewFromEnv(db, mode, concurrency)
	if err != nil {
		return true, err
	}
	migrator.SetProgress(func(done, total int, bytes int64) {
		if done == total || done%100 == 0 {
			fmt.Fprintf(
				os.Stderr,
				"storage migration progress: objects=%d/%d bytes=%d\n",
				done,
				total,
				bytes,
			)
		}
	})
	report, runErr := migrator.Run(ctx)
	encoded, marshalErr := json.MarshalIndent(report, "", "  ")
	if marshalErr == nil {
		fmt.Fprintln(os.Stdout, string(encoded))
	}
	if runErr != nil {
		return true, runErr
	}
	return true, nil
}
