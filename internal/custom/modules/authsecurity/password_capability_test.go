package authsecurity

import (
	"context"
	"net/http"
	"testing"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newPasswordCapabilityDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"),
		&gorm.Config{},
	)
	require.NoError(t, err)
	require.NoError(t, db.Exec(`
		CREATE TABLE custom_iam_users (
			id TEXT PRIMARY KEY,
			weknora_user_id TEXT,
			deleted_at DATETIME
		)
	`).Error)
	return db
}

func TestGetPasswordCapabilityDistinguishesLocalAndIAMAccounts(t *testing.T) {
	db := newPasswordCapabilityDB(t)
	service := NewService(db, nil, Config{})
	ctx := context.Background()

	local, err := service.GetPasswordCapability(ctx, "local-user")
	require.NoError(t, err)
	require.Equal(t, "local", local.AccountSource)
	require.True(t, local.CanChangePassword)
	require.Empty(t, local.Reason)

	require.NoError(t, db.Exec(
		"INSERT INTO custom_iam_users (id, weknora_user_id) VALUES (?, ?)",
		"iam-link",
		"iam-user",
	).Error)
	iamAccount, err := service.GetPasswordCapability(ctx, "iam-user")
	require.NoError(t, err)
	require.Equal(t, "iam", iamAccount.AccountSource)
	require.False(t, iamAccount.CanChangePassword)
	require.NotEmpty(t, iamAccount.Reason)

	require.NoError(t, db.Exec(
		"UPDATE custom_iam_users SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?",
		"iam-link",
	).Error)
	unlinked, err := service.GetPasswordCapability(ctx, "iam-user")
	require.NoError(t, err)
	require.Equal(t, "local", unlinked.AccountSource)
	require.True(t, unlinked.CanChangePassword)
}

func TestGuardPasswordChangeEnforcesIAMPolicyServerSide(t *testing.T) {
	db := newPasswordCapabilityDB(t)
	require.NoError(t, db.Exec(
		"INSERT INTO custom_iam_users (id, weknora_user_id) VALUES (?, ?)",
		"iam-link",
		"iam-user",
	).Error)
	handler := NewHandler(NewService(db, nil, Config{}))

	err := handler.GuardPasswordChange(
		context.Background(),
		&types.User{ID: "iam-user"},
	)
	var appErr *apperrors.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusForbidden, appErr.HTTPCode)

	require.NoError(t, handler.GuardPasswordChange(
		context.Background(),
		&types.User{ID: "local-user"},
	))
}
