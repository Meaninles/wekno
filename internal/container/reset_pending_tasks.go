package container

import (
	"context"
	"os"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

const resetPendingStaleWindow = 30 * time.Minute

// resetPendingTasks closes stale data-source sync logs left behind by an
// unexpected application restart.
//
// Knowledge parse, summary, finalization, and deletion rows are deliberately
// excluded here. Those lifecycles have durable Asynq/PostgreSQL recovery and
// the queue-aware HousekeepingService. Mutating them during database startup
// happens before either recovery component can inspect liveness and previously
// turned valid backlogged/finalizing documents into failed rows. In particular,
// parse_status=deleting is itself the durable delete intent and must survive a
// crash so wikidelete.Recovery can republish the worker task.
func resetPendingTasks(db *gorm.DB) {
	distributed := os.Getenv("REDIS_ADDR") != ""

	var staleCutoff time.Time
	if distributed {
		staleCutoff = time.Now().Add(-resetPendingStaleWindow)
	}

	// Data-source sync does not yet have the document lifecycle's durable
	// recovery contract, so retain its historical startup cleanup.
	now := time.Now()
	resultSync := stuckSyncLogQuery(db, distributed, staleCutoff).Updates(map[string]interface{}{
		"status":        types.SyncLogStatusFailed,
		"error_message": "Sync interrupted due to application restart",
		"finished_at":   &now,
	})
	if resultSync.Error != nil {
		logger.Warnf(context.Background(), "Failed to reset pending data source sync tasks: %v", resultSync.Error)
	} else if resultSync.RowsAffected > 0 {
		logger.Infof(context.Background(),
			"Reset %d stuck data source sync tasks to failed state (distributed=%v)",
			resultSync.RowsAffected, distributed)
	}
}

func stuckSyncLogQuery(db *gorm.DB, distributed bool, staleCutoff time.Time) *gorm.DB {
	q := db.Model(&types.SyncLog{}).
		Where("status = ?", types.SyncLogStatusRunning)
	if distributed {
		q = q.Where("started_at < ?", staleCutoff)
	}
	return q
}
