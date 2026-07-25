package knowledgeaux

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestDefaultRecoveryDoesNotRunLegacyBackfillOnEveryPod(t *testing.T) {
	t.Setenv("KNOWLEDGE_AUX_LEGACY_BACKFILL_ENABLED", "")
	config := DefaultRecoveryConfig()
	require.False(t, config.BackfillEnabled)
	require.Positive(t, config.InitialDelay)
}

func TestPostgresRecoveryLeadershipIsExclusiveAndTransferable(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("WEKNORA_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("WEKNORA_TEST_POSTGRES_DSN is not configured")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	first := NewRecovery(&Registry{db: db})
	second := NewRecovery(&Registry{db: db})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	firstLeadership, acquired, err := first.tryLeadership(ctx)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NotNil(t, firstLeadership)
	t.Cleanup(func() {
		if firstLeadership != nil {
			firstLeadership.release()
		}
	})

	secondLeadership, acquired, err := second.tryLeadership(ctx)
	require.NoError(t, err)
	require.False(t, acquired)
	require.Nil(t, secondLeadership)

	firstLeadership.release()
	firstLeadership = nil
	require.Eventually(t, func() bool {
		secondLeadership, acquired, err = second.tryLeadership(ctx)
		return err == nil && acquired
	}, 2*time.Second, 25*time.Millisecond)
	require.NotNil(t, secondLeadership)
	secondLeadership.release()
}
