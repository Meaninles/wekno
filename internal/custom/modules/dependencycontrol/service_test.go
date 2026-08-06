package dependencycontrol

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/runtimeprofile"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestExactParadeDBCorruptionOpensPersistentCircuit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:dependency-control?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	service := NewService(db, runtimeprofile.Profile{})
	require.NoError(t, service.Migrate(context.Background()))

	cause := fmt.Errorf("stage split part vector batch: %w", &pgconn.PgError{
		Code:    "XX000",
		Message: "expected to deserialize valid SegmentMetaEntryHeader: UnexpectedEnd",
	})
	observed := service.Observe(
		context.Background(), CapabilityKeywordIndex, KeywordIndexScope, cause,
	)
	require.ErrorIs(t, observed, ErrDependencyDeferred)
	delay, ok := RetryAfter(observed)
	require.True(t, ok)
	require.Positive(t, delay)

	var stored Capability
	require.NoError(t, db.First(&stored,
		"capability = ? AND scope = ?", CapabilityKeywordIndex, KeywordIndexScope,
	).Error)
	require.Equal(t, StateBlocked, stored.State)
	require.Equal(t, "keyword_index_corrupt", stored.LastErrorCode)
	require.NotEmpty(t, stored.IncidentID)
	require.Contains(t, stored.LastErrorMessage, "SegmentMetaEntryHeader")
}

func TestUnrelatedPostgresInternalErrorRemainsBusinessVisible(t *testing.T) {
	service := NewService(nil, runtimeprofile.Profile{})
	cause := &pgconn.PgError{Code: "XX000", Message: "unrelated internal invariant"}
	observed := service.Observe(
		context.Background(), CapabilityKeywordIndex, KeywordIndexScope, cause,
	)
	require.Same(t, cause, observed)
	require.False(t, errors.Is(observed, ErrDependencyDeferred))
}

func TestPostgresRestartErrorIsBudgetFreeDeferral(t *testing.T) {
	service := NewService(nil, runtimeprofile.Profile{})
	cause := &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}
	observed := service.Observe(
		context.Background(), CapabilityKeywordIndex, KeywordIndexScope, cause,
	)
	require.ErrorIs(t, observed, ErrDependencyDeferred)
	var deferred *DeferredError
	require.ErrorAs(t, observed, &deferred)
	require.Equal(t, CapabilityPostgres, deferred.Capability)
	require.Equal(t, "postgres_unavailable", deferred.Code)
}
