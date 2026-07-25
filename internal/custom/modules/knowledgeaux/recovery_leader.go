package knowledgeaux

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

const (
	recoveryLeadershipRetry = 5 * time.Second
	// "WKNORAUX" encoded as a positive signed 64-bit advisory-lock key.
	postgresRecoveryLockKey int64 = 0x574B4E4F52415558
	mysqlRecoveryLockName         = "weknora:knowledge-aux:recovery"
)

type recoveryLeadership struct {
	conn    *sql.Conn
	dialect string
}

func (l *recoveryLeadership) alive(ctx context.Context) error {
	if l == nil || l.conn == nil {
		return nil
	}
	var one int
	return l.conn.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (l *recoveryLeadership) release() {
	if l == nil || l.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	switch l.dialect {
	case "postgres":
		var released bool
		_ = l.conn.QueryRowContext(
			ctx,
			"SELECT pg_advisory_unlock($1)",
			postgresRecoveryLockKey,
		).Scan(&released)
	case "mysql":
		var released sql.NullInt64
		_ = l.conn.QueryRowContext(
			ctx,
			"SELECT RELEASE_LOCK(?)",
			mysqlRecoveryLockName,
		).Scan(&released)
	}
	_ = l.conn.Close()
}

func (r *Recovery) tryLeadership(
	ctx context.Context,
) (*recoveryLeadership, bool, error) {
	if r == nil || r.registry == nil || r.registry.db == nil {
		return nil, false, errors.New("knowledge auxiliary recovery database is unavailable")
	}
	dialect := r.registry.db.Dialector.Name()
	if dialect != "postgres" && dialect != "mysql" {
		// SQLite/Lite deployments are single-process by design. The existing
		// process-local scan mutex remains sufficient there.
		return &recoveryLeadership{dialect: dialect}, true, nil
	}
	sqlDB, err := r.registry.db.DB()
	if err != nil {
		return nil, false, fmt.Errorf("get recovery SQL pool: %w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("reserve recovery leadership connection: %w", err)
	}
	acquired := false
	switch dialect {
	case "postgres":
		err = conn.QueryRowContext(
			ctx,
			"SELECT pg_try_advisory_lock($1)",
			postgresRecoveryLockKey,
		).Scan(&acquired)
	case "mysql":
		var value sql.NullInt64
		err = conn.QueryRowContext(
			ctx,
			"SELECT GET_LOCK(?, 0)",
			mysqlRecoveryLockName,
		).Scan(&value)
		acquired = value.Valid && value.Int64 == 1
	}
	if err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("acquire recovery leadership: %w", err)
	}
	if !acquired {
		_ = conn.Close()
		return nil, false, nil
	}
	return &recoveryLeadership{conn: conn, dialect: dialect}, true, nil
}

func (r *Recovery) runLeaderLoop(ctx context.Context) {
	for ctx.Err() == nil {
		leadership, acquired, err := r.tryLeadership(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Errorf(ctx, "[knowledge aux] acquire recovery leadership failed: %v", err)
			}
		} else if acquired {
			logger.Infof(ctx, "[knowledge aux] recovery leadership acquired")
			r.runWhileLeader(ctx, leadership)
			leadership.release()
			if ctx.Err() != nil {
				return
			}
		}
		retry := time.NewTimer(recoveryLeadershipRetry)
		select {
		case <-ctx.Done():
			if !retry.Stop() {
				<-retry.C
			}
			return
		case <-retry.C:
		}
	}
}

func (r *Recovery) runWhileLeader(
	ctx context.Context,
	leadership *recoveryLeadership,
) {
	initial := time.NewTimer(r.config.InitialDelay)
	select {
	case <-ctx.Done():
		if !initial.Stop() {
			<-initial.C
		}
		return
	case <-initial.C:
	}

	run := func() bool {
		healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := leadership.alive(healthCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				logger.Warnf(ctx, "[knowledge aux] recovery leadership connection lost: %v", err)
			}
			return false
		}
		r.runCycle(ctx)
		return true
	}
	if !run() {
		return
	}
	ticker := time.NewTicker(r.config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !run() {
				return
			}
		}
	}
}
