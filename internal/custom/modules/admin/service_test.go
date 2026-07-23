package admin

import (
	"context"
	"testing"
	"time"

	appservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type accountCreationUserService struct {
	interfaces.UserService
	register func(context.Context, *types.RegisterRequest) (*types.User, error)
}

func (s *accountCreationUserService) Register(
	ctx context.Context,
	req *types.RegisterRequest,
) (*types.User, error) {
	return s.register(ctx, req)
}

func newAccountCreationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"),
		&gorm.Config{},
	)
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	require.NoError(t, db.Exec(`
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			deleted_at DATETIME
		)
	`).Error)
	require.NoError(t, db.Exec(`
		CREATE TABLE custom_iam_users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			deleted_at DATETIME
		)
	`).Error)
	require.NoError(t, db.Exec(`
		CREATE TABLE tenants (
			id INTEGER PRIMARY KEY,
			name TEXT,
			deleted_at DATETIME
		)
	`).Error)
	return db
}

func TestCreateLocalAccountGeneratesCompliantRandomPassword(t *testing.T) {
	db := newAccountCreationDB(t)
	require.NoError(t, db.Exec(
		"INSERT INTO tenants (id, name) VALUES (?, ?)",
		42,
		"测试账号的空间",
	).Error)

	var captured *types.RegisterRequest
	provisioned := false
	userService := &accountCreationUserService{
		register: func(_ context.Context, req *types.RegisterRequest) (*types.User, error) {
			copied := *req
			captured = &copied
			now := time.Now().UTC()
			return &types.User{
				ID:          "user-1",
				Username:    req.Username,
				DisplayName: req.DisplayName,
				TenantID:    42,
				IsActive:    true,
				CreatedAt:   now,
				UpdatedAt:   now,
			}, nil
		},
	}
	service := NewService(db, userService)
	service.SetProvisioner(func(_ context.Context, user *types.User) error {
		provisioned = user != nil && user.ID == "user-1"
		return nil
	})

	result, err := service.CreateLocalAccount(context.Background(), CreateLocalAccountRequest{
		Username:    " test_user ",
		DisplayName: " 测试用户 ",
	})
	require.NoError(t, err)
	require.NotNil(t, captured)
	require.Equal(t, "test_user", captured.Username)
	require.Equal(t, "测试用户", captured.DisplayName)
	require.NotEmpty(t, captured.Password)
	require.NoError(t, appservice.ValidatePasswordComplexity(captured.Password))
	require.Equal(t, captured.Password, result.TemporaryPassword)
	require.Equal(t, "测试账号的空间", result.User.TenantName)
	require.True(t, provisioned)
}

func TestCreateLocalAccountRejectsLocalAndIAMUsernameCollisions(t *testing.T) {
	tests := []struct {
		name      string
		insertSQL string
		wantErr   error
	}{
		{
			name:      "local username is case insensitive",
			insertSQL: "INSERT INTO users (id, username) VALUES ('local-1', 'ExistingUser')",
			wantErr:   ErrLocalUsernameExists,
		},
		{
			name:      "IAM username is reserved",
			insertSQL: "INSERT INTO custom_iam_users (id, username) VALUES ('iam-1', 'ExistingUser')",
			wantErr:   ErrIAMUsernameExists,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newAccountCreationDB(t)
			require.NoError(t, db.Exec(tt.insertSQL).Error)
			userService := &accountCreationUserService{
				register: func(context.Context, *types.RegisterRequest) (*types.User, error) {
					t.Fatal("Register must not run for a colliding username")
					return nil, nil
				},
			}

			_, err := NewService(db, userService).CreateLocalAccount(
				context.Background(),
				CreateLocalAccountRequest{Username: "existinguser"},
			)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestCreateLocalAccountValidatesUsername(t *testing.T) {
	db := newAccountCreationDB(t)
	service := NewService(db, &accountCreationUserService{
		register: func(context.Context, *types.RegisterRequest) (*types.User, error) {
			t.Fatal("Register must not run for invalid input")
			return nil, nil
		},
	})

	for _, username := range []string{"a", "contains space", "user@example.com", "123456789012345678901"} {
		_, err := service.CreateLocalAccount(
			context.Background(),
			CreateLocalAccountRequest{Username: username},
		)
		require.ErrorIs(t, err, ErrInvalidUsername, username)
	}
}
