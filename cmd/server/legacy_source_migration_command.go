package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/legacysourcemigration"
	"github.com/Tencent/WeKnora/internal/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const legacySourceMigrationApplyConfirmation = "--confirm-old-objects-retained"

func runLegacySourceMigrationCommand(
	ctx context.Context,
	args []string,
) (bool, error) {
	if len(args) == 0 || args[0] != "legacy-source-migrate" {
		return false, nil
	}
	if len(args) < 2 {
		return true, errors.New(
			"usage: WeKnora legacy-source-migrate audit | " +
				"WeKnora legacy-source-migrate apply " +
				legacySourceMigrationApplyConfirmation,
		)
	}
	mode := legacysourcemigration.Mode(strings.ToLower(strings.TrimSpace(args[1])))
	switch mode {
	case legacysourcemigration.ModeAudit:
	case legacysourcemigration.ModeApply:
		confirmed := false
		for _, arg := range args[2:] {
			if arg == legacySourceMigrationApplyConfirmation {
				confirmed = true
				break
			}
		}
		if !confirmed {
			return true, fmt.Errorf(
				"apply requires %s to acknowledge that historical OBS objects "+
					"will be retained after row-fenced cutover",
				legacySourceMigrationApplyConfirmation,
			)
		}
	default:
		return true, fmt.Errorf("unsupported legacy source migration mode %q", mode)
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

	migrator, err := legacysourcemigration.NewFromEnv(db, mode)
	if err != nil {
		return true, err
	}
	migrator.SetProgress(func(done, total int, verifiedBytes int64) {
		if done == total || done%25 == 0 {
			fmt.Fprintf(
				os.Stderr,
				"legacy source migration progress: documents=%d/%d verified_bytes=%d\n",
				done,
				total,
				verifiedBytes,
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
