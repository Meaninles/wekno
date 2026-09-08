package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type accountJWTRejector struct {
	interfaces.UserService
	calls int
}

func (s *accountJWTRejector) ValidateToken(context.Context, string) (*types.User, uint64, error) {
	s.calls++
	return nil, 0, errors.New("not an account JWT")
}

func TestPostgresBudgetThroughGlobalAuthAndInternalKeyValidation(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_API_KEY", "internal-test-key")
	db := testsupport.Postgres(t, &RunRecord{}, &ModelRequest{})
	row := fixtureRun(t, db)
	gin.SetMode(gin.TestMode)
	accounts := &accountJWTRejector{}
	router := gin.New()
	router.Use(middleware.Auth(nil, accounts, nil, nil))
	router.POST("/api/v1/custom/agent-runtime/internal/runs/:operation", NewHandler(&Service{db: db}).RunControl)
	for _, item := range []struct {
		path, key string
		status    int
	}{
		{"budget", "internal-test-key", 200},
		{"budget", "wrong-key", 401},
		{"budget", "", 401},
		{"budget/extra", "internal-test-key", 401},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/custom/agent-runtime/internal/runs/"+item.path, strings.NewReader(`{"run_id":"`+row.ID+`","owner_epoch":2}`))
		req.Header.Set("Content-Type", "application/json")
		if item.key != "" {
			req.Header.Set("Authorization", "Bearer "+item.key)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, item.status, rec.Code, rec.Body.String())
		if item.status == 200 {
			var budget BudgetState
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &budget))
			require.EqualValues(t, 3000000, budget.RemainingTokens)
		}
	}
	require.Equal(t, 1, accounts.calls, "Only the unregistered sibling path should reach account JWT authentication")
}
