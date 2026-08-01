package capacitycontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
)

func validPool() modeladmission.ResourcePool {
	return modeladmission.ResourcePool{
		ID: "pool-chat", Name: "Qwen", ResourceKind: "chat",
		MaxInflight: 4, MaxBackgroundInflight: 3, InteractiveReserve: 1,
		TenantGuaranteed: 1, TenantBurst: 4,
		DocumentGuaranteed: 1, DocumentBurst: 2,
		RequestTimeoutSeconds: 900,
		CircuitThreshold:      3, CircuitWindowSeconds: 600, CircuitOpenSeconds: 60,
		State: "enabled", PolicyVersion: 1,
	}
}

func TestCompilePoolReportsEffectiveModuleGrants(t *testing.T) {
	row := CompilePool(validPool(), []modeladmission.ResourceBinding{{
		ModelID: "model-1", ModelTenantID: 1, ResourcePoolID: "pool-chat",
		RouteFingerprint: "same-physical-route",
	}})
	require.Equal(t, 3, row.Effective.BackgroundMax)
	require.Equal(t, 2, row.Effective.DocumentMax)
	require.Equal(t, 4, row.Effective.ChatSessions)
	require.Len(t, row.ModuleGrants, 4)
	require.Equal(t, 2, row.ModuleGrants[1].Effective)
	require.NotContains(t, severities(row.Issues), "error")
}

func TestCapacityConflictMatrix(t *testing.T) {
	tests := []struct {
		name string
		edit func(*modeladmission.ResourcePool)
		code string
	}{
		{"reserve consumes pool", func(row *modeladmission.ResourcePool) { row.InteractiveReserve = row.MaxInflight }, "invalid_interactive_reserve"},
		{"materialized background differs", func(row *modeladmission.ResourcePool) { row.MaxBackgroundInflight = 4 }, "background_materialization_mismatch"},
		{"tenant exceeds total", func(row *modeladmission.ResourcePool) { row.TenantBurst = 5 }, "tenant_exceeds_pool"},
		{"document exceeds background", func(row *modeladmission.ResourcePool) { row.DocumentBurst = 4 }, "document_exceeds_upstream"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := validPool()
			test.edit(&pool)
			row := CompilePool(pool, nil)
			require.Contains(t, issueCodes(row.Issues), test.code)
		})
	}
}

func TestValidateEndpointCanonicalizesRedundantFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pool := validPool()
	pool.MaxInflight = 6
	pool.InteractiveReserve = 2
	pool.MaxBackgroundInflight = 99
	pool.TenantGuaranteed = 3
	pool.DocumentGuaranteed = 2
	pool.TokenBurst = 999
	chatConcurrent := 100
	pool.ChatMaxConcurrent = &chatConcurrent
	body, err := json.Marshal(pool)
	require.NoError(t, err)

	router := gin.New()
	router.POST("/validate", NewHandler(nil).Validate)
	request := httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Data validationResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Data.Valid)
	require.NotNil(t, response.Data.Canonical)
	require.Equal(t, 4, response.Data.Canonical.MaxBackgroundInflight)
	require.Nil(t, response.Data.Canonical.ChatMaxConcurrent)
	require.Equal(t, 1, response.Data.Canonical.TenantGuaranteed)
	require.EqualValues(t, 0, response.Data.Canonical.TokenBurst)
}

func TestCompileDetectsOneRouteSplitAcrossPools(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&modeladmission.ResourcePool{}, &modeladmission.ResourceBinding{},
	))
	now := time.Now().UTC()
	first := validPool()
	first.CreatedAt, first.UpdatedAt = now, now
	second := first
	second.ID, second.Name = "pool-chat-2", "Qwen duplicate"
	require.NoError(t, db.Create(&first).Error)
	require.NoError(t, db.Create(&second).Error)
	require.NoError(t, db.Create([]modeladmission.ResourceBinding{
		{ModelID: "model-1", ModelTenantID: 1, ResourcePoolID: first.ID, RouteFingerprint: "route-a", BindingVersion: 1, CreatedAt: now, UpdatedAt: now},
		{ModelID: "model-2", ModelTenantID: 1, ResourcePoolID: second.ID, RouteFingerprint: "route-a", BindingVersion: 1, CreatedAt: now, UpdatedAt: now},
	}).Error)

	report, err := NewService(db, nil).Compile(context.Background())
	require.NoError(t, err)
	require.False(t, report.Healthy)
	require.Contains(t, issueCodes(report.Issues), "route_split_across_pools")
}

func issueCodes(issues []Issue) []string {
	values := make([]string, 0, len(issues))
	for _, issue := range issues {
		values = append(values, issue.Code)
	}
	return values
}

func severities(issues []Issue) []string {
	values := make([]string, 0, len(issues))
	for _, issue := range issues {
		values = append(values, issue.Severity)
	}
	return values
}
