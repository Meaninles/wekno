package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
)

type reparseSnapshotKnowledgeServiceFake struct {
	interfaces.KnowledgeService
	rows []*types.Knowledge
}

func (s *reparseSnapshotKnowledgeServiceFake) GetKnowledgeBatch(
	context.Context, uint64, []string,
) ([]*types.Knowledge, error) {
	return s.rows, nil
}

type reparseSnapshotEnqueuerFake struct {
	task *asynq.Task
	opts []asynq.Option
	err  error
}

func (e *reparseSnapshotEnqueuerFake) Enqueue(
	task *asynq.Task, opts ...asynq.Option,
) (*asynq.TaskInfo, error) {
	e.task = task
	e.opts = append([]asynq.Option(nil), opts...)
	if e.err != nil {
		return nil, e.err
	}
	return &asynq.TaskInfo{ID: "parent-task", Type: task.Type()}, nil
}

func TestEnqueueKnowledgeListReparsePersistsExpectedSnapshotsInParentPayload(t *testing.T) {
	rows := []*types.Knowledge{
		{
			ID: "knowledge-b", TenantID: 7, KnowledgeBaseID: "kb-1",
			ParseStatus: types.ParseStatusFailed, ProcessingGeneration: "generation-b",
			UpdatedAt: time.Date(2026, 7, 22, 1, 2, 3, 2000, time.UTC),
		},
		{
			ID: "knowledge-a", TenantID: 7, KnowledgeBaseID: "kb-1",
			ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "generation-a",
			UpdatedAt: time.Date(2026, 7, 22, 1, 2, 3, 1000, time.UTC),
		},
	}
	enqueuer := &reparseSnapshotEnqueuerFake{}
	handler := &KnowledgeHandler{
		kgService:   &reparseSnapshotKnowledgeServiceFake{rows: rows},
		asynqClient: enqueuer,
	}
	ids := []string{"knowledge-a", "knowledge-b"}
	if _, err := handler.enqueueKnowledgeListReparse(
		context.Background(), 7, "kb-1", ids, nil,
	); err != nil {
		t.Fatal(err)
	}
	if enqueuer.task == nil {
		t.Fatal("parent task was not enqueued")
	}
	var queue string
	for _, opt := range enqueuer.opts {
		if opt.Type() == asynq.QueueOpt {
			queue, _ = opt.Value().(string)
		}
	}
	if queue != types.QueueCritical {
		t.Fatalf("parent reparse queue = %q, want %q", queue, types.QueueCritical)
	}
	var payload types.KnowledgeListReparsePayload
	if err := json.Unmarshal(enqueuer.task.Payload(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.ExpectedSnapshots) != len(ids) {
		t.Fatalf("snapshots = %d, want %d", len(payload.ExpectedSnapshots), len(ids))
	}
	for _, row := range rows {
		snapshot, ok := payload.ExpectedSnapshots[row.ID]
		if !ok || !processownership.BatchReparseSnapshotMatches(row, snapshot) {
			t.Fatalf("snapshot for %s does not match durable source row: %#v", row.ID, snapshot)
		}
	}
	if len(payload.KnowledgeIDs) != len(ids) || payload.KnowledgeIDs[0] != ids[0] || payload.KnowledgeIDs[1] != ids[1] {
		t.Fatalf("knowledge order changed: %#v", payload.KnowledgeIDs)
	}
}

type directReparseKnowledgeServiceFake struct {
	interfaces.KnowledgeService
	knowledge        *types.Knowledge
	getBatchCalls    int
	directCalls      int
	batchTenantID    uint64
	batchKnowledgeID string
}

func (s *directReparseKnowledgeServiceFake) GetKnowledgeByIDOnly(
	context.Context, string,
) (*types.Knowledge, error) {
	return s.knowledge, nil
}

func (s *directReparseKnowledgeServiceFake) GetKnowledgeBatch(
	_ context.Context, tenantID uint64, ids []string,
) ([]*types.Knowledge, error) {
	s.getBatchCalls++
	s.batchTenantID = tenantID
	if len(ids) == 1 {
		s.batchKnowledgeID = ids[0]
	}
	return []*types.Knowledge{s.knowledge}, nil
}

func (s *directReparseKnowledgeServiceFake) ReparseKnowledge(
	context.Context, string, *types.KnowledgeProcessOverrides,
) (*types.Knowledge, error) {
	s.directCalls++
	return s.knowledge, nil
}

func TestReparseKnowledgeUsesDurableSingleDocumentController(t *testing.T) {
	gin.SetMode(gin.TestMode)
	knowledge := &types.Knowledge{
		ID:              "knowledge-1",
		TenantID:        7,
		KnowledgeBaseID: "kb-1",
		ParseStatus:     types.ParseStatusCompleted,
		UpdatedAt:       time.Date(2026, 7, 22, 3, 4, 5, 6000, time.UTC),
	}
	service := &directReparseKnowledgeServiceFake{knowledge: knowledge}
	enqueuer := &reparseSnapshotEnqueuerFake{}
	handler := &KnowledgeHandler{kgService: service, asynqClient: enqueuer}

	router := gin.New()
	router.Use(middleware.ErrorHandler())
	router.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(7))
		c.Next()
	})
	router.POST("/knowledge/:id/reparse", handler.ReparseKnowledge)

	body := bytes.NewBufferString(`{"process_config":{"graph_enabled":false}}`)
	request := httptest.NewRequest(http.MethodPost, "/knowledge/knowledge-1/reparse", body)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if service.directCalls != 0 {
		t.Fatalf("direct ReparseKnowledge calls = %d, want 0", service.directCalls)
	}
	if service.getBatchCalls != 1 || service.batchTenantID != 7 || service.batchKnowledgeID != knowledge.ID {
		t.Fatalf("snapshot capture calls=%d tenant=%d knowledge=%q", service.getBatchCalls, service.batchTenantID, service.batchKnowledgeID)
	}
	if enqueuer.task == nil || enqueuer.task.Type() != types.TypeKnowledgeListReparse {
		t.Fatalf("queued task = %#v, want %s", enqueuer.task, types.TypeKnowledgeListReparse)
	}

	var payload types.KnowledgeListReparsePayload
	if err := json.Unmarshal(enqueuer.task.Payload(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.BatchID == "" || payload.ProcessingGeneration == "" || payload.ProcessingOwner == "" {
		t.Fatalf("single-document durable identity is incomplete: %#v", payload)
	}
	if payload.ExpectedSnapshot == nil || payload.ExpectedSnapshot.KnowledgeID != knowledge.ID {
		t.Fatalf("single-document expected snapshot = %#v", payload.ExpectedSnapshot)
	}
	if payload.ProcessConfig == nil || payload.ProcessConfig.GraphEnabled == nil || *payload.ProcessConfig.GraphEnabled {
		t.Fatalf("process override was not preserved: %#v", payload.ProcessConfig)
	}

	var responseBody struct {
		Success bool             `json:"success"`
		Data    *types.Knowledge `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &responseBody); err != nil {
		t.Fatal(err)
	}
	if !responseBody.Success || responseBody.Data == nil || responseBody.Data.ID != knowledge.ID {
		t.Fatalf("response compatibility changed: %s", response.Body.String())
	}
}

func TestReparseKnowledgeQueueFailureDoesNotStartDestructiveServicePath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	knowledge := &types.Knowledge{
		ID:              "knowledge-1",
		TenantID:        7,
		KnowledgeBaseID: "kb-1",
		ParseStatus:     types.ParseStatusCompleted,
		UpdatedAt:       time.Date(2026, 7, 22, 3, 4, 5, 6000, time.UTC),
	}
	service := &directReparseKnowledgeServiceFake{knowledge: knowledge}
	enqueuer := &reparseSnapshotEnqueuerFake{err: context.Canceled}
	handler := &KnowledgeHandler{kgService: service, asynqClient: enqueuer}

	router := gin.New()
	router.Use(middleware.ErrorHandler())
	router.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(7))
		c.Next()
	})
	router.POST("/knowledge/:id/reparse", handler.ReparseKnowledge)

	request := httptest.NewRequest(http.MethodPost, "/knowledge/knowledge-1/reparse", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
	if service.directCalls != 0 {
		t.Fatalf("direct ReparseKnowledge calls = %d, want 0", service.directCalls)
	}
	if enqueuer.task == nil || enqueuer.task.Type() != types.TypeKnowledgeListReparse {
		t.Fatalf("queue submission was not attempted with the durable task: %#v", enqueuer.task)
	}
}
