package kbmanager

import (
	"context"
	"fmt"
	"mime/multipart"
	"sync"
	"testing"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type kbManagerWorkflowKnowledgeService struct {
	interfaces.KnowledgeService
	mu              sync.Mutex
	documents       map[string]*types.Knowledge
	nextStatus      string
	createdID       string
	deletedIDs      []string
	createStartedCh chan struct{}
}

func (s *kbManagerWorkflowKnowledgeService) CreateKnowledgeFromFile(
	_ context.Context,
	kbID string,
	file *multipart.FileHeader,
	_ map[string]string,
	_ *bool,
	customFileName string,
	_ []string,
	_ string,
	_ *types.KnowledgeProcessOverrides,
) (*types.Knowledge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := customFileName
	if name == "" && file != nil {
		name = file.Filename
	}
	status := s.nextStatus
	if status == "" {
		status = types.ParseStatusProcessing
	}
	created := &types.Knowledge{
		ID:              s.createdID,
		TenantID:        1,
		KnowledgeBaseID: kbID,
		Type:            "file",
		FileName:        name,
		FileHash:        "new-hash",
		ParseStatus:     status,
	}
	s.documents[created.ID] = created
	if s.createStartedCh != nil {
		select {
		case <-s.createStartedCh:
		default:
			close(s.createStartedCh)
		}
	}
	clone := *created
	return &clone, nil
}

func (s *kbManagerWorkflowKnowledgeService) GetKnowledgeByIDOnly(_ context.Context, id string) (*types.Knowledge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.documents[id]
	if doc == nil {
		return nil, nil
	}
	clone := *doc
	return &clone, nil
}

func (s *kbManagerWorkflowKnowledgeService) DeleteKnowledge(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.documents[id] == nil {
		return fmt.Errorf("document %s not found", id)
	}
	delete(s.documents, id)
	s.deletedIDs = append(s.deletedIDs, id)
	return nil
}

func (s *kbManagerWorkflowKnowledgeService) GetKnowledgeTags(_ context.Context, _ []string) (map[string][]*types.KnowledgeTag, error) {
	return map[string][]*types.KnowledgeTag{}, nil
}

func (s *kbManagerWorkflowKnowledgeService) setStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.documents[id] != nil {
		s.documents[id].ParseStatus = status
	}
}

func (s *kbManagerWorkflowKnowledgeService) exists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.documents[id] != nil
}

func (s *kbManagerWorkflowKnowledgeService) wasDeleted(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, deleted := range s.deletedIDs {
		if deleted == id {
			return true
		}
	}
	return false
}

type kbManagerWorkflowTenantService struct {
	interfaces.TenantService
}

func (kbManagerWorkflowTenantService) GetTenantByID(_ context.Context, id uint64) (*types.Tenant, error) {
	return &types.Tenant{ID: id}, nil
}

type kbManagerWorkflowFileResolver struct{}

func (kbManagerWorkflowFileResolver) ResolveRunFile(_ context.Context, runID, sourceType, sourceID string) ([]byte, string, string, error) {
	if runID == "" || sourceType != "artifact" || sourceID != "artifact-1" {
		return nil, "", "", fmt.Errorf("unexpected source")
	}
	return []byte("replacement content"), "replacement.md", "source-sha", nil
}

func newKBManagerWorkflowService(t *testing.T, nextStatus string) (*Service, *kbManagerWorkflowKnowledgeService, ToolScope, context.Context) {
	t.Helper()
	dsn := fmt.Sprintf("file:kbmanager-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Operation{}, &types.CustomAgent{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	kbService := &kbManagerTestKBService{kbs: map[string]*types.KnowledgeBase{
		"kb-a": {ID: "kb-a", Name: "A", Type: types.KnowledgeBaseTypeDocument, TenantID: 1},
	}}
	knowledgeService := &kbManagerWorkflowKnowledgeService{
		documents: map[string]*types.Knowledge{
			"old-doc": {
				ID:              "old-doc",
				TenantID:        1,
				KnowledgeBaseID: "kb-a",
				Type:            "file",
				FileName:        "old.md",
				FileHash:        "old-hash",
				ParseStatus:     types.ParseStatusCompleted,
			},
		},
		nextStatus:      nextStatus,
		createdID:       "new-doc",
		createStartedCh: make(chan struct{}),
	}
	service := NewService(
		db,
		kbService,
		knowledgeService,
		nil,
		kbManagerWorkflowTenantService{},
		kbManagerWorkflowFileResolver{},
	)
	agent := &types.CustomAgent{
		ID:       "agent-1",
		TenantID: 1,
		Name:     "manager",
		Config: types.CustomAgentConfig{
			AgentMode:       types.AgentModeSmartReasoning,
			AgentType:       types.AgentTypeKnowledgeBaseManager,
			KBSelectionMode: "selected",
			KnowledgeBases:  []string{"kb-a"},
			KnowledgeManagement: &types.KnowledgeManagementConfig{
				DefaultPermissions: types.KnowledgeManagementPermissionSet{Add: true, Delete: true},
			},
		},
	}
	if err := db.Create(agent).Error; err != nil {
		t.Fatalf("create agent: %v", err)
	}
	runtimeScope := &types.KnowledgeManagementRuntimeScope{
		WholeKnowledgeBaseIDs: []string{"kb-a"},
		EffectivePermissions: map[string]types.KnowledgeManagementPermissionSet{
			"kb-a": {Add: true, Delete: true, Modify: true},
		},
	}
	scope := ToolScope{AgentID: agent.ID, AgentTenantID: 1, SessionID: "session-1", Runtime: runtimeScope}
	ctx := kbManagerTestContext(1)
	ctx = context.WithValue(ctx, types.UserIDContextKey, "system-1")
	ctx = agenttools.WithToolExecContext(ctx, &agenttools.ToolExecContext{RunID: "run-1", SessionID: "session-1"})
	return service, knowledgeService, scope, ctx
}

func waitForKBManagerOperation(t *testing.T, service *Service, ctx context.Context, scope ToolScope, operationID string) *Operation {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		operation, err := service.GetOperation(ctx, scope, operationID, 100*time.Millisecond)
		if err != nil {
			t.Fatalf("GetOperation: %v", err)
		}
		if operation.Terminal() {
			return operation
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("operation %s did not reach terminal state", operationID)
	return nil
}

func TestReplaceWritesNewDocumentButNeverDeletesOldInBackend(t *testing.T) {
	service, knowledgeService, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusProcessing)
	operation, err := service.ReplaceDocument(ctx, scope, ReplaceDocumentRequest{
		KnowledgeID:         "old-doc",
		ExpectedOldFileHash: "old-hash",
		Source:              FileSource{SourceType: "artifact", SourceID: "artifact-1", FileName: "replacement.md"},
	})
	if err != nil {
		t.Fatalf("ReplaceDocument() error = %v", err)
	}
	<-knowledgeService.createStartedCh
	if !knowledgeService.exists("new-doc") {
		t.Fatal("replacement document was not written")
	}
	if !knowledgeService.exists("old-doc") || knowledgeService.wasDeleted("old-doc") {
		t.Fatal("backend must never delete the old document")
	}

	knowledgeService.setStatus("new-doc", types.ParseStatusCompleted)
	terminal := waitForKBManagerOperation(t, service, ctx, scope, operation.ID)
	if terminal.State != OperationStateCompleted {
		t.Fatalf("terminal state = %s, want completed; error=%s", terminal.State, terminal.ErrorMessage)
	}
	if !knowledgeService.exists("old-doc") || knowledgeService.wasDeleted("old-doc") {
		t.Fatal("backend deleted the old document after parsing completed")
	}
	if !knowledgeService.exists("new-doc") {
		t.Fatal("new replacement document is missing")
	}
}

func TestAgentCanDeleteOldImmediatelyAfterReplacementWrite(t *testing.T) {
	service, knowledgeService, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusProcessing)
	_, err := service.ReplaceDocument(ctx, scope, ReplaceDocumentRequest{
		KnowledgeID:         "old-doc",
		ExpectedOldFileHash: "old-hash",
		Source:              FileSource{SourceType: "artifact", SourceID: "artifact-1", FileName: "replacement.md"},
	})
	if err != nil {
		t.Fatalf("ReplaceDocument() error = %v", err)
	}
	deleteOperation, err := service.DeleteDocument(ctx, scope, DeleteDocumentRequest{
		KnowledgeID:      "old-doc",
		ExpectedFileHash: "old-hash",
	})
	if err != nil {
		t.Fatalf("DeleteDocument() error = %v", err)
	}
	if deleteOperation.State != OperationStateCompleted {
		t.Fatalf("delete state = %s, want completed", deleteOperation.State)
	}
	if knowledgeService.exists("old-doc") || !knowledgeService.wasDeleted("old-doc") {
		t.Fatal("agent-initiated delete did not remove the old document immediately")
	}
	if !knowledgeService.exists("new-doc") {
		t.Fatal("replacement document disappeared")
	}
}

func TestDocumentSelectionDoesNotGrantStandaloneAdd(t *testing.T) {
	service, _, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusCompleted)
	scope.Runtime.WholeKnowledgeBaseIDs = nil
	scope.Runtime.Documents = map[string]string{"old-doc": "kb-a"}
	_, err := service.AddDocument(ctx, scope, AddDocumentRequest{
		KnowledgeBaseID: "kb-a",
		Source:          FileSource{SourceType: "artifact", SourceID: "artifact-1", FileName: "new.md"},
	})
	if err == nil || err.Error() != "standalone add is outside the current turn scope; select the whole target knowledge base" {
		t.Fatalf("AddDocument() error = %v", err)
	}
}
