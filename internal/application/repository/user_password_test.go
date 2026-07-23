package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestChangePasswordAndRevokeTokensIsAtomic(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"),
		&gorm.Config{},
	)
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.User{}, &types.AuthToken{}))

	now := time.Now().UTC()
	user := &types.User{
		ID:           "user-1",
		Username:     "local-user",
		PasswordHash: "old-hash",
		IsActive:     true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, db.Create(user).Error)
	require.NoError(t, db.Create([]types.AuthToken{
		{
			ID:        "access-1",
			UserID:    user.ID,
			Token:     "access-token",
			TokenType: "access_token",
			ExpiresAt: now.Add(time.Hour),
		},
		{
			ID:        "refresh-1",
			UserID:    user.ID,
			Token:     "refresh-token",
			TokenType: "refresh_token",
			ExpiresAt: now.Add(24 * time.Hour),
		},
		{
			ID:        "other-access",
			UserID:    "other-user",
			Token:     "other-token",
			TokenType: "access_token",
			ExpiresAt: now.Add(time.Hour),
		},
	}).Error)

	repository := NewUserRepository(db)
	changedAt := now.Add(time.Minute)
	require.NoError(t, repository.ChangePasswordAndRevokeTokens(
		context.Background(),
		user.ID,
		"old-hash",
		"new-hash",
		changedAt,
	))

	var updated types.User
	require.NoError(t, db.First(&updated, "id = ?", user.ID).Error)
	require.Equal(t, "new-hash", updated.PasswordHash)

	var activeOwnTokens int64
	require.NoError(t, db.Model(&types.AuthToken{}).
		Where("user_id = ? AND is_revoked = ?", user.ID, false).
		Count(&activeOwnTokens).Error)
	require.Zero(t, activeOwnTokens)

	var activeOtherTokens int64
	require.NoError(t, db.Model(&types.AuthToken{}).
		Where("user_id = ? AND is_revoked = ?", "other-user", false).
		Count(&activeOtherTokens).Error)
	require.EqualValues(t, 1, activeOtherTokens)

	require.ErrorIs(t, repository.ChangePasswordAndRevokeTokens(
		context.Background(),
		user.ID,
		"old-hash",
		"third-hash",
		time.Now().UTC(),
	), ErrPasswordChanged)
	require.NoError(t, db.First(&updated, "id = ?", user.ID).Error)
	require.Equal(t, "new-hash", updated.PasswordHash)
}

func TestChangePasswordRollsBackWhenTokenRevocationFails(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"),
		&gorm.Config{},
	)
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.User{}))

	user := &types.User{
		ID:           "user-rollback",
		Username:     "rollback-user",
		PasswordHash: "old-hash",
		IsActive:     true,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)

	repository := NewUserRepository(db)
	require.Error(t, repository.ChangePasswordAndRevokeTokens(
		context.Background(),
		user.ID,
		"old-hash",
		"new-hash",
		time.Now().UTC(),
	))

	var unchanged types.User
	require.NoError(t, db.First(&unchanged, "id = ?", user.ID).Error)
	require.Equal(t, "old-hash", unchanged.PasswordHash)
}
