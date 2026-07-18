package kbmanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"gorm.io/gorm"
)

const managementChannel = "agent_knowledge_manager"

// RunFileResolver is implemented by the General Agent runtime. Only two
// current-run source kinds are accepted: a registered artifact token or a
// byte-verified uploaded/selected input-file ID.
type RunFileResolver interface {
	ResolveRunFile(ctx context.Context, runID, sourceType, sourceID string) ([]byte, string, string, error)
}

type Service struct {
	db               *gorm.DB
	kbService        interfaces.KnowledgeBaseService
	knowledgeService interfaces.KnowledgeService
	kbShareService   interfaces.KBShareService
	tenantService    interfaces.TenantService
	fileResolver     RunFileResolver
	configurator     *Configurator

	startOnce sync.Once
	runningMu sync.Mutex
	running   map[string]bool
}

func NewService(
	db *gorm.DB,
	kbService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	kbShareService interfaces.KBShareService,
	tenantService interfaces.TenantService,
	fileResolver RunFileResolver,
) *Service {
	return &Service{
		db:               db,
		kbService:        kbService,
		knowledgeService: knowledgeService,
		kbShareService:   kbShareService,
		tenantService:    tenantService,
		fileResolver:     fileResolver,
		configurator:     NewConfigurator(kbService, knowledgeService, kbShareService),
		running:          make(map[string]bool),
	}
}

func (s *Service) Configurator() *Configurator { return s.configurator }

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).AutoMigrate(&Operation{})
}

func (s *Service) Start() {
	if s == nil || s.db == nil {
		return
	}
	s.startOnce.Do(func() {
		s.resumePending()
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				s.resumePending()
			}
		}()
	})
}

type FileSource struct {
	SourceType string `json:"source_type" jsonschema:"artifact or input_file; URLs and filesystem paths are forbidden"`
	SourceID   string `json:"source_id" jsonschema:"file_token returned by create_artifact, or id from input_files/original_input_manifest.json"`
	FileName   string `json:"file_name,omitempty" jsonschema:"required for artifact sources; optional override for input_file sources; keep a knowledge-base-supported extension"`
}

type AddDocumentRequest struct {
	KnowledgeBaseID string            `json:"knowledge_base_id" jsonschema:"target knowledge base ID from kb_list_documents or visible runtime scope"`
	Source          FileSource        `json:"source"`
	Metadata        map[string]string `json:"metadata,omitempty" jsonschema:"optional document metadata; provenance and operation audit fields are added by the server"`
	TagIDs          []string          `json:"tag_ids,omitempty" jsonschema:"optional tag IDs belonging to the target knowledge base"`
	Reason          string            `json:"reason,omitempty" jsonschema:"concise user-facing reason for the mutation"`
}

type ReplaceDocumentRequest struct {
	KnowledgeID         string            `json:"knowledge_id" jsonschema:"old document ID to replace"`
	Source              FileSource        `json:"source"`
	ExpectedOldFileHash string            `json:"expected_old_file_hash,omitempty" jsonschema:"optional optimistic-concurrency hash returned by kb_list_documents"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	TagIDs              []string          `json:"tag_ids,omitempty" jsonschema:"optional new tags; when omitted the old document tags are preserved"`
	Reason              string            `json:"reason,omitempty"`
}

type DeleteDocumentRequest struct {
	KnowledgeID      string `json:"knowledge_id" jsonschema:"document ID to delete"`
	ExpectedFileHash string `json:"expected_file_hash,omitempty" jsonschema:"optional optimistic-concurrency hash returned by kb_list_documents"`
	Reason           string `json:"reason,omitempty"`
}

type operationActor struct {
	runID          string
	sessionID      string
	agentID        string
	agentTenantID  uint64
	callerTenantID uint64
	userID         string
	role           types.TenantRole
}

func actorFromContext(ctx context.Context, scope ToolScope) (operationActor, error) {
	meta, ok := agenttools.ToolExecFromContext(ctx)
	if !ok || strings.TrimSpace(meta.RunID) == "" {
		return operationActor{}, fmt.Errorf("knowledge mutation requires an active General Agent run")
	}
	callerTenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || callerTenantID == 0 {
		return operationActor{}, fmt.Errorf("caller tenant is unavailable")
	}
	userID, ok := types.UserIDFromContext(ctx)
	if !ok || userID == "" {
		return operationActor{}, fmt.Errorf("caller identity is unavailable")
	}
	return operationActor{
		runID:          meta.RunID,
		sessionID:      scope.SessionID,
		agentID:        scope.AgentID,
		agentTenantID:  scope.AgentTenantID,
		callerTenantID: callerTenantID,
		userID:         userID,
		role:           types.TenantRoleFromContext(ctx),
	}, nil
}

func (s *Service) AddDocument(ctx context.Context, scope ToolScope, input AddDocumentRequest) (*Operation, error) {
	kbID := strings.TrimSpace(input.KnowledgeBaseID)
	if scope.Runtime == nil || !scope.Runtime.HasWholeKnowledgeBase(kbID) {
		return nil, fmt.Errorf("standalone add is outside the current turn scope; select the whole target knowledge base")
	}
	if !scope.Runtime.PermissionsFor(kbID).Add {
		return nil, fmt.Errorf("add permission is not effective for knowledge base %s", kbID)
	}
	actor, err := actorFromContext(ctx, scope)
	if err != nil {
		return nil, err
	}
	kb, sourceCtx, err := s.authorizeMutation(ctx, kbID)
	if err != nil {
		return nil, err
	}
	data, fileName, sha, err := s.resolveSource(ctx, actor.runID, input.Source)
	if err != nil {
		return nil, err
	}
	operation := s.newOperation(actor, kb, OperationTypeAdd, input.Source, fileName, sha, input.Reason)
	if err := s.db.WithContext(ctx).Create(operation).Error; err != nil {
		return nil, err
	}
	metadata := managedMetadata(input.Metadata, operation, "")
	knowledge, createErr := s.createKnowledge(sourceCtx, kbID, data, fileName, metadata, compactUnique(input.TagIDs))
	if duplicate := duplicateKnowledge(createErr); duplicate != nil {
		return s.finishDuplicate(ctx, operation, duplicate, "相同文件已存在，未重复新增")
	}
	if createErr != nil || knowledge == nil {
		if createErr == nil {
			createErr = fmt.Errorf("native ingestion returned no document")
		}
		return s.failOperation(ctx, operation, fmt.Errorf("新增文档失败: %w", createErr))
	}
	operation.NewKnowledgeID = knowledge.ID
	operation.State = OperationStateParsing
	operation.ResultMessage = "文档已添加到知识库，原生解析、索引和派生处理将在后台继续"
	if err := s.saveOperation(ctx, operation); err != nil {
		return nil, err
	}
	s.kick(operation.ID)
	return operation, nil
}

func (s *Service) ReplaceDocument(ctx context.Context, scope ToolScope, input ReplaceDocumentRequest) (*Operation, error) {
	old, err := s.knowledgeService.GetKnowledgeByIDOnly(ctx, strings.TrimSpace(input.KnowledgeID))
	if err != nil || old == nil {
		return nil, fmt.Errorf("old document does not exist")
	}
	if scope.Runtime == nil || !scope.Runtime.ContainsDocument(old.ID, old.KnowledgeBaseID) {
		return nil, fmt.Errorf("document is outside the current turn scope")
	}
	if !scope.Runtime.PermissionsFor(old.KnowledgeBaseID).Modify {
		return nil, fmt.Errorf("modify requires both effective add and delete permissions for this knowledge base")
	}
	if expected := strings.TrimSpace(input.ExpectedOldFileHash); expected != "" && expected != old.FileHash {
		return nil, fmt.Errorf("old document changed after inspection; refresh inventory before replacing it")
	}
	actor, err := actorFromContext(ctx, scope)
	if err != nil {
		return nil, err
	}
	kb, sourceCtx, err := s.authorizeMutation(ctx, old.KnowledgeBaseID)
	if err != nil {
		return nil, err
	}
	data, fileName, sha, err := s.resolveSource(ctx, actor.runID, input.Source)
	if err != nil {
		return nil, err
	}
	operation := s.newOperation(actor, kb, OperationTypeReplace, input.Source, fileName, sha, input.Reason)
	operation.OldKnowledgeID = old.ID
	operation.OldFileHash = old.FileHash
	if err := s.db.WithContext(ctx).Create(operation).Error; err != nil {
		return nil, err
	}
	tagIDs := compactUnique(input.TagIDs)
	if len(tagIDs) == 0 {
		tagIDs = s.knowledgeTagIDs(sourceCtx, old.ID)
	}
	metadata := managedMetadata(input.Metadata, operation, old.ID)
	knowledge, createErr := s.createKnowledge(sourceCtx, old.KnowledgeBaseID, data, fileName, metadata, tagIDs)
	if duplicate := duplicateKnowledge(createErr); duplicate != nil {
		if duplicate.ID == old.ID {
			operation.NewKnowledgeID = old.ID
			operation.State = OperationStateNoChange
			operation.ResultMessage = "新文件与旧文档完全相同，未执行替换"
			now := time.Now()
			operation.CompletedAt = &now
			return operation, s.saveOperation(ctx, operation)
		}
		return s.finishDuplicate(ctx, operation, duplicate, "新文件已作为其他文档存在；为避免误删旧文档，未执行替换")
	}
	if createErr != nil || knowledge == nil {
		if createErr == nil {
			createErr = fmt.Errorf("native ingestion returned no document")
		}
		return s.failOperation(ctx, operation, fmt.Errorf("创建替换文档失败，旧文档已保留: %w", createErr))
	}
	operation.NewKnowledgeID = knowledge.ID
	operation.State = OperationStateParsing
	operation.ResultMessage = "替换用的新文档已添加到知识库；此工具不会删除旧文档，智能体现在可以显式调用删除工具"
	if err := s.saveOperation(ctx, operation); err != nil {
		return nil, err
	}
	s.kick(operation.ID)
	return operation, nil
}

func (s *Service) DeleteDocument(ctx context.Context, scope ToolScope, input DeleteDocumentRequest) (*Operation, error) {
	knowledge, err := s.knowledgeService.GetKnowledgeByIDOnly(ctx, strings.TrimSpace(input.KnowledgeID))
	if err != nil || knowledge == nil {
		return nil, fmt.Errorf("document does not exist")
	}
	if scope.Runtime == nil || !scope.Runtime.ContainsDocument(knowledge.ID, knowledge.KnowledgeBaseID) {
		return nil, fmt.Errorf("document is outside the current turn scope")
	}
	if !scope.Runtime.PermissionsFor(knowledge.KnowledgeBaseID).Delete {
		return nil, fmt.Errorf("delete permission is not effective for this knowledge base")
	}
	if expected := strings.TrimSpace(input.ExpectedFileHash); expected != "" && expected != knowledge.FileHash {
		return nil, fmt.Errorf("document changed after inspection; refresh inventory before deleting it")
	}
	actor, err := actorFromContext(ctx, scope)
	if err != nil {
		return nil, err
	}
	kb, sourceCtx, err := s.authorizeMutation(ctx, knowledge.KnowledgeBaseID)
	if err != nil {
		return nil, err
	}
	operation := s.newOperation(actor, kb, OperationTypeDelete, FileSource{}, knowledge.FileName, "", input.Reason)
	operation.OldKnowledgeID = knowledge.ID
	operation.OldFileHash = knowledge.FileHash
	if err := s.db.WithContext(ctx).Create(operation).Error; err != nil {
		return nil, err
	}
	if err := s.knowledgeService.DeleteKnowledge(sourceCtx, knowledge.ID); err != nil {
		return s.failOperation(ctx, operation, fmt.Errorf("删除文档失败: %w", err))
	}
	now := time.Now()
	operation.State = OperationStateCompleted
	operation.CompletedAt = &now
	operation.ResultMessage = "文档及其派生索引、Wiki 来源引用和文档级知识图谱数据已删除"
	return operation, s.saveOperation(ctx, operation)
}

func (s *Service) resolveSource(ctx context.Context, runID string, source FileSource) ([]byte, string, string, error) {
	if s.fileResolver == nil {
		return nil, "", "", fmt.Errorf("current-run file resolver is unavailable")
	}
	sourceType := strings.TrimSpace(source.SourceType)
	if sourceType != "artifact" && sourceType != "input_file" {
		return nil, "", "", fmt.Errorf("source_type must be artifact or input_file; URL/path ingestion is forbidden")
	}
	data, trustedName, sha, err := s.fileResolver.ResolveRunFile(ctx, runID, sourceType, strings.TrimSpace(source.SourceID))
	if err != nil {
		return nil, "", "", err
	}
	fileName := strings.TrimSpace(source.FileName)
	if strings.TrimSpace(trustedName) != "" {
		if fileName != "" && fileName != strings.TrimSpace(trustedName) {
			return nil, "", "", fmt.Errorf("file_name must match the trusted current-run file name %q", trustedName)
		}
		fileName = strings.TrimSpace(trustedName)
	} else if fileName == "" {
		fileName = strings.TrimSpace(trustedName)
	}
	if fileName == "" {
		return nil, "", "", fmt.Errorf("file_name is required for artifact sources")
	}
	validated, ok := secutils.ValidateInput(filepath.Base(fileName))
	if !ok || strings.TrimSpace(filepath.Ext(validated)) == "" {
		return nil, "", "", fmt.Errorf("file_name must be safe and include a supported extension")
	}
	safeName, err := secutils.SafeFileName(validated)
	if err != nil {
		return nil, "", "", err
	}
	return data, safeName, sha, nil
}

func (s *Service) createKnowledge(
	ctx context.Context,
	kbID string,
	data []byte,
	fileName string,
	metadata map[string]string,
	tagIDs []string,
) (*types.Knowledge, error) {
	file, cleanup, err := bytesToFileHeader(data, fileName)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return s.knowledgeService.CreateKnowledgeFromFile(ctx, kbID, file, metadata, nil, fileName, tagIDs, managementChannel, nil)
}

func bytesToFileHeader(data []byte, fileName string) (*multipart.FileHeader, func(), error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, func() {}, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, func() {}, err
	}
	if err := writer.Close(); err != nil {
		return nil, func() {}, err
	}
	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, err := reader.ReadForm(int64(len(data)) + 1024)
	if err != nil {
		return nil, func() {}, err
	}
	files := form.File["file"]
	if len(files) != 1 {
		_ = form.RemoveAll()
		return nil, func() {}, fmt.Errorf("failed to materialize upload file")
	}
	return files[0], func() { _ = form.RemoveAll() }, nil
}

func (s *Service) authorizeMutation(ctx context.Context, kbID string) (*types.KnowledgeBase, context.Context, error) {
	permission := s.configurator.platformMutationPermissions(ctx, kbID)
	if !permission.Add || !permission.Delete {
		return nil, nil, fmt.Errorf("current caller no longer has platform mutation permission for this knowledge base")
	}
	kb, err := s.kbService.GetKnowledgeBaseByIDOnly(ctx, kbID)
	if err != nil || kb == nil {
		return nil, nil, fmt.Errorf("knowledge base does not exist")
	}
	tenant, err := s.tenantService.GetTenantByID(ctx, kb.TenantID)
	if err != nil || tenant == nil {
		return nil, nil, fmt.Errorf("source tenant is unavailable")
	}
	sourceCtx := context.WithValue(ctx, types.TenantIDContextKey, kb.TenantID)
	sourceCtx = context.WithValue(sourceCtx, types.TenantInfoContextKey, tenant)
	return kb, sourceCtx, nil
}

func (s *Service) newOperation(actor operationActor, kb *types.KnowledgeBase, operationType string, source FileSource, fileName, sha, reason string) *Operation {
	return &Operation{
		AgentID:         actor.agentID,
		AgentTenantID:   actor.agentTenantID,
		SessionID:       actor.sessionID,
		RunID:           actor.runID,
		CallerTenantID:  actor.callerTenantID,
		SourceTenantID:  kb.TenantID,
		UserID:          actor.userID,
		CallerRole:      string(actor.role),
		Type:            operationType,
		State:           OperationStatePreparing,
		KnowledgeBaseID: kb.ID,
		SourceKind:      strings.TrimSpace(source.SourceType),
		SourceID:        strings.TrimSpace(source.SourceID),
		SourceSHA256:    sha,
		FileName:        fileName,
		Reason:          strings.TrimSpace(reason),
	}
}

func managedMetadata(input map[string]string, operation *Operation, replacesID string) map[string]string {
	out := make(map[string]string, len(input)+6)
	for key, value := range input {
		key = strings.TrimSpace(key)
		if key != "" {
			out[key] = value
		}
	}
	out["managed_by"] = "knowledge-base-manager-agent"
	out["management_operation_id"] = operation.ID
	out["management_action"] = operation.Type
	out["management_source_sha256"] = operation.SourceSHA256
	if operation.Reason != "" {
		out["management_reason"] = operation.Reason
	}
	if replacesID != "" {
		out["replaces_knowledge_id"] = replacesID
	}
	return out
}

func duplicateKnowledge(err error) *types.Knowledge {
	var duplicate *types.DuplicateKnowledgeError
	if errors.As(err, &duplicate) && duplicate != nil {
		return duplicate.Knowledge
	}
	return nil
}

func (s *Service) knowledgeTagIDs(ctx context.Context, knowledgeID string) []string {
	if s.knowledgeService == nil {
		return nil
	}
	tagMap, err := s.knowledgeService.GetKnowledgeTags(ctx, []string{knowledgeID})
	if err != nil {
		return nil
	}
	var out []string
	for _, tag := range tagMap[knowledgeID] {
		if tag != nil {
			out = append(out, tag.ID)
		}
	}
	return compactUnique(out)
}

func (s *Service) saveOperation(ctx context.Context, operation *Operation) error {
	if operation == nil {
		return fmt.Errorf("operation is nil")
	}
	operation.UpdatedAt = time.Now()
	return s.db.WithContext(ctx).Save(operation).Error
}

func (s *Service) failOperation(ctx context.Context, operation *Operation, err error) (*Operation, error) {
	operation.State = OperationStateFailed
	operation.ErrorMessage = err.Error()
	now := time.Now()
	operation.CompletedAt = &now
	if saveErr := s.saveOperation(ctx, operation); saveErr != nil {
		return operation, saveErr
	}
	return operation, err
}

func (s *Service) finishDuplicate(ctx context.Context, operation *Operation, duplicate *types.Knowledge, message string) (*Operation, error) {
	if duplicate != nil {
		operation.NewKnowledgeID = duplicate.ID
	}
	operation.State = OperationStateDuplicate
	operation.ResultMessage = message
	now := time.Now()
	operation.CompletedAt = &now
	return operation, s.saveOperation(ctx, operation)
}

func (s *Service) resumePending() {
	var operations []Operation
	if err := s.db.Where("state = ?", OperationStateParsing).Find(&operations).Error; err != nil {
		logger.Warnf(context.Background(), "[kbmanager] resume pending operations failed: %v", err)
		return
	}
	for i := range operations {
		s.kick(operations[i].ID)
	}
}

func (s *Service) kick(operationID string) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return
	}
	s.runningMu.Lock()
	if s.running[operationID] {
		s.runningMu.Unlock()
		return
	}
	s.running[operationID] = true
	s.runningMu.Unlock()
	go func() {
		defer func() {
			s.runningMu.Lock()
			delete(s.running, operationID)
			s.runningMu.Unlock()
		}()
		s.monitor(operationID)
	}()
}

func (s *Service) monitor(operationID string) {
	ctx := context.Background()
	for {
		var operation Operation
		if err := s.db.WithContext(ctx).First(&operation, "id = ?", operationID).Error; err != nil {
			return
		}
		if operation.Terminal() {
			return
		}
		knowledge, err := s.knowledgeService.GetKnowledgeByIDOnly(ctx, operation.NewKnowledgeID)
		if err != nil || knowledge == nil {
			_, _ = s.failOperation(ctx, &operation, fmt.Errorf("new document disappeared before parsing completed"))
			return
		}
		switch knowledge.ParseStatus {
		case types.ParseStatusCompleted:
			now := time.Now()
			operation.State = OperationStateCompleted
			operation.CompletedAt = &now
			if operation.Type == OperationTypeReplace {
				operation.ResultMessage = "替换用的新文档后台解析及派生处理已完成；旧文档删除只由智能体的独立删除调用决定"
			} else {
				operation.ResultMessage = "新文档解析、索引及已启用的派生知识处理均已完成"
			}
			_ = s.saveOperation(ctx, &operation)
			return
		case types.ParseStatusFailed, types.ParseStatusCancelled, types.ParseStatusDeleting:
			_, _ = s.failOperation(ctx, &operation, fmt.Errorf("new document background parsing ended in state %s", knowledge.ParseStatus))
			return
		}
		time.Sleep(3 * time.Second)
	}
}

func (s *Service) GetOperation(ctx context.Context, scope ToolScope, operationID string, wait time.Duration) (*Operation, error) {
	deadline := time.Now().Add(wait)
	for {
		var operation Operation
		err := s.db.WithContext(ctx).
			Where("id = ? AND agent_id = ?", strings.TrimSpace(operationID), scope.AgentID).
			First(&operation).Error
		if err != nil {
			return nil, err
		}
		callerTenantID, _ := types.TenantIDFromContext(ctx)
		userID, _ := types.UserIDFromContext(ctx)
		if operation.CallerTenantID != callerTenantID || operation.UserID != userID {
			return nil, fmt.Errorf("operation is outside the current caller scope")
		}
		if scope.Runtime == nil || (!scope.Runtime.HasWholeKnowledgeBase(operation.KnowledgeBaseID) &&
			scope.Runtime.Documents[operation.OldKnowledgeID] != operation.KnowledgeBaseID) {
			return nil, fmt.Errorf("operation is outside the current turn scope")
		}
		if operation.Terminal() || wait <= 0 || time.Now().After(deadline) {
			return &operation, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}
