package kbmanager

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	db *gorm.DB
	interfaces.KnowledgeService
	mu              sync.Mutex
	documents       map[string]*types.Knowledge
	nextStatus      string
	createdID       string
	deletedIDs      []string
	createStartedCh chan struct{}
}

func (s *kbManagerWorkflowKnowledgeService) CreateKnowledgeFromFile(
	ctx context.Context,
	kbID string,
	file *multipart.FileHeader,
	metadata map[string]string,
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
		PublicationState: types.KnowledgePublicationFromContext(ctx), ProcessingGeneration: "generation-new",
		ID:              s.createdID,
		TenantID:        1,
		KnowledgeBaseID: kbID,
		Type:            "file",
		FileName:        name,
		FileHash:        "new-hash",
		ParseStatus:     status,
	}
	created.Metadata, _ = json.Marshal(metadata)
	if status == types.ParseStatusCompleted {
		created.CoreStatus = types.CoreStatusReady
	}
	if err := s.db.Create(created).Error; err != nil {
		return nil, err
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
	if err := s.db.Model(&types.Knowledge{}).Where("id = ?", id).Updates(map[string]any{"core_status": doc.CoreStatus, "enrichment_status": doc.EnrichmentStatus, "file_hash": doc.FileHash, "processing_generation": doc.ProcessingGeneration}).Error; err != nil {
		return nil, err
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
	if err := s.db.Unscoped().Where("id = ?", id).Delete(&types.Knowledge{}).Error; err != nil {
		return err
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
		if status == types.ParseStatusCompleted {
			s.documents[id].CoreStatus = types.CoreStatusReady
		}
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
	data := []byte("replacement content")
	return data, "replacement.md", fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func newKBManagerWorkflowService(t *testing.T, nextStatus string) (*Service, *kbManagerWorkflowKnowledgeService, ToolScope, context.Context) {
	t.Helper()
	dsn := fmt.Sprintf("file:kbmanager-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Operation{}, &OperationInput{}, &types.WikiPage{}, &types.TaskPendingOp{}, &types.CustomAgent{}, &types.Knowledge{}, &types.KnowledgeBase{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	kbService := &kbManagerTestKBService{kbs: map[string]*types.KnowledgeBase{
		"kb-a": {ID: "kb-a", Name: "A", Type: types.KnowledgeBaseTypeDocument, TenantID: 1},
	}}
	knowledgeService := &kbManagerWorkflowKnowledgeService{
		db: db,
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
	if err := db.Create(kbService.kbs["kb-a"]).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(knowledgeService.documents["old-doc"]).Error; err != nil {
		t.Fatal(err)
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
			AgentMode:       types.AgentModeUnified,
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
	t.Cleanup(service.Stop)
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

func TestRepairReplaceRetainsOldUntilRequiredIndexesReady(t *testing.T) {
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
	if knowledgeService.exists("old-doc") || !knowledgeService.wasDeleted("old-doc") {
		t.Fatal("old document was not cleaned after new required indexes completed")
	}
	if !knowledgeService.exists("new-doc") {
		t.Fatal("new replacement document is missing")
	}
}

func TestRepairReplacementOwnsDeletion(t *testing.T) {
	service, knowledgeService, scope, ctx := newKBManagerWorkflowService(t, types.ParseStatusProcessing)
	_, err := service.ReplaceDocument(ctx, scope, ReplaceDocumentRequest{
		KnowledgeID:         "old-doc",
		ExpectedOldFileHash: "old-hash",
		Source:              FileSource{SourceType: "artifact", SourceID: "artifact-1", FileName: "replacement.md"},
	})
	if err != nil {
		t.Fatalf("ReplaceDocument() error = %v", err)
	}
	_, err = service.DeleteDocument(ctx, scope, DeleteDocumentRequest{KnowledgeID: "old-doc", ExpectedFileHash: "old-hash"})
	if err == nil {
		t.Fatal("independent delete bypassed active replacement")
	}
	if !knowledgeService.exists("old-doc") {
		t.Fatal("old document disappeared before replacement was ready")
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
