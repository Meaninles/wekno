package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/fileguard"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var createKnowledgeAuxDBSequence atomic.Uint64

type createKnowledgeFileRepoStub struct {
	interfaces.KnowledgeRepository

	createCalls      int
	createErr        error
	createdKnowledge *types.Knowledge
	processingCAS    int
	events           *[]string
	lastCASValues    map[string]interface{}
	forceCASConflict bool
}

func (r *createKnowledgeFileRepoStub) CheckKnowledgeExists(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	params *types.KnowledgeCheckParams,
) (bool, *types.Knowledge, error) {
	return false, nil, nil
}

func (r *createKnowledgeFileRepoStub) CreateKnowledge(ctx context.Context, knowledge *types.Knowledge) error {
	if r.events != nil {
		*r.events = append(*r.events, "create")
	}
	r.createCalls++
	copied := *knowledge
	r.createdKnowledge = &copied
	return r.createErr
}

func (r *createKnowledgeFileRepoStub) CreateKnowledgeTx(
	_ context.Context, tx *gorm.DB, knowledge *types.Knowledge,
) error {
	if r.events != nil {
		*r.events = append(*r.events, "create")
	}
	r.createCalls++
	copied := *knowledge
	r.createdKnowledge = &copied
	if r.createErr != nil {
		return r.createErr
	}
	return tx.Create(knowledge).Error
}

// GetKnowledgeTags is invoked by setAndAttachKnowledgeTags after create even
// when no tags were supplied; a fresh knowledge has none, so return empty.
func (r *createKnowledgeFileRepoStub) GetKnowledgeTags(
	ctx context.Context,
	knowledgeIDs []string,
) (map[string][]*types.KnowledgeTag, error) {
	return map[string][]*types.KnowledgeTag{}, nil
}

func (r *createKnowledgeFileRepoStub) GetKnowledgeByID(
	_ context.Context, tenantID uint64, id string,
) (*types.Knowledge, error) {
	if r.createdKnowledge == nil || r.createdKnowledge.TenantID != tenantID || r.createdKnowledge.ID != id {
		return nil, errors.New("knowledge not found")
	}
	copied := *r.createdKnowledge
	return &copied, nil
}

func (r *createKnowledgeFileRepoStub) CompareAndSwapDocumentProcessing(
	_ context.Context,
	tenantID uint64,
	id string,
	knowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	expectedOwner string,
	values map[string]interface{},
) (bool, error) {
	r.processingCAS++
	r.lastCASValues = values
	if r.forceCASConflict {
		return false, nil
	}
	knowledge := r.createdKnowledge
	if knowledge == nil || knowledge.TenantID != tenantID || knowledge.ID != id ||
		knowledge.KnowledgeBaseID != knowledgeBaseID || knowledge.ParseStatus != expectedParseStatus ||
		knowledge.ProcessingGeneration != expectedGeneration || knowledge.ProcessingOwner != expectedOwner {
		return false, nil
	}
	if status, ok := values["parse_status"].(string); ok {
		knowledge.ParseStatus = status
	}
	if message, ok := values["error_message"].(string); ok {
		knowledge.ErrorMessage = message
	}
	if owner, ok := values["processing_owner"].(string); ok {
		knowledge.ProcessingOwner = owner
	}
	if workflowID, ok := values["processing_workflow_id"].(string); ok {
		knowledge.ProcessingWorkflowID = workflowID
	}
	if _, ok := values["processing_fanout"]; ok {
		knowledge.ProcessingFanout = nil
	}
	if count, ok := values["pending_subtasks_count"].(int); ok {
		knowledge.PendingSubtasksCount = count
	}
	return true, nil
}

type createKnowledgeFileKBServiceStub struct {
	interfaces.KnowledgeBaseService

	kb *types.KnowledgeBase
}

func (s *createKnowledgeFileKBServiceStub) GetKnowledgeBaseByID(
	ctx context.Context,
	id string,
) (*types.KnowledgeBase, error) {
	return s.kb, nil
}

type createKnowledgeFileServiceStub struct {
	saveErr              error
	saveCalls            int
	savedWithKnowledgeID string
	deleteCalls          int
	deletedPath          string
}

func (s *createKnowledgeFileServiceStub) ReserveFilePath(
	_ uint64, knowledgeID string, _ string,
) (string, error) {
	s.savedWithKnowledgeID = knowledgeID
	return "stored/" + knowledgeID, nil
}

func (s *createKnowledgeFileServiceStub) CommitFileAtPath(
	_ context.Context, _ *multipart.FileHeader, _ string,
) error {
	s.saveCalls++
	return s.saveErr
}

func (s *createKnowledgeFileServiceStub) ReserveBytesPath(uint64, string, bool) (string, error) {
	return "", errors.New("not implemented")
}

func (s *createKnowledgeFileServiceStub) CommitBytesAtPath(context.Context, []byte, string) error {
	return errors.New("not implemented")
}

func (s *createKnowledgeFileServiceStub) ReserveCopyPath(string, uint64, string) (string, error) {
	return "", errors.New("not implemented")
}

func (s *createKnowledgeFileServiceStub) CommitCopyAtPath(context.Context, string, string) error {
	return errors.New("not implemented")
}

func (s *createKnowledgeFileServiceStub) CheckConnectivity(ctx context.Context) error {
	return nil
}

func (s *createKnowledgeFileServiceStub) SaveFile(
	ctx context.Context,
	file *multipart.FileHeader,
	tenantID uint64,
	knowledgeID string,
) (string, error) {
	s.saveCalls++
	s.savedWithKnowledgeID = knowledgeID
	if s.saveErr != nil {
		return "", s.saveErr
	}
	return "stored/" + knowledgeID, nil
}

func (s *createKnowledgeFileServiceStub) SaveBytes(
	ctx context.Context,
	data []byte,
	tenantID uint64,
	fileName string,
	temp bool,
) (string, error) {
	return "", errors.New("not implemented")
}

func (s *createKnowledgeFileServiceStub) GetFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (s *createKnowledgeFileServiceStub) GetFileURL(ctx context.Context, filePath string) (string, error) {
	return "", errors.New("not implemented")
}

func (s *createKnowledgeFileServiceStub) DeleteFile(ctx context.Context, filePath string) error {
	s.deleteCalls++
	s.deletedPath = filePath
	return nil
}

func (s *createKnowledgeFileServiceStub) CopyFile(ctx context.Context, srcPath string, tenantID uint64, knowledgeID string) (string, error) {
	return "", errors.New("not implemented")
}

type createKnowledgeTaskEnqueuerStub struct {
	calls        int
	prepareCalls int
	abortCalls   int
	queues       []string
	err          error
	payloads     [][]byte
	taskTypes    []string
	events       *[]string
}

func configureCreateKnowledgeAuxiliary(
	t *testing.T, svc *knowledgeService, fileSvc *createKnowledgeFileServiceStub,
) {
	t.Helper()
	kbSvc := svc.kbService.(*createKnowledgeFileKBServiceStub)
	kbSvc.kb.TenantID = 1
	kbSvc.kb.SetStorageProvider("local")
	dsn := fmt.Sprintf("file:create-knowledge-aux-%d?mode=memory&cache=shared", createKnowledgeAuxDBSequence.Add(1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.Tenant{}, &types.KnowledgeBase{}, &types.Knowledge{}, &types.TaskPendingOp{},
	))
	require.NoError(t, db.Create(&types.Tenant{ID: 1, Name: "tenant"}).Error)
	require.NoError(t, db.Create(kbSvc.kb).Error)
	svc.auxObjects = knowledgeaux.NewWithResolver(db, func(
		context.Context, *types.Tenant, string,
	) (interfaces.FileService, string, error) {
		return fileSvc, "local", nil
	})
}

func (s *createKnowledgeTaskEnqueuerStub) Enqueue(
	task *asynq.Task,
	opts ...asynq.Option,
) (*asynq.TaskInfo, error) {
	s.calls++
	queue := types.QueueDefault
	for _, opt := range opts {
		if opt.Type() == asynq.QueueOpt {
			if q, ok := opt.Value().(string); ok && q != "" {
				queue = q
			}
		}
	}
	s.queues = append(s.queues, queue)
	if s.err != nil {
		return nil, s.err
	}
	return &asynq.TaskInfo{ID: "task-1", Queue: queue}, nil
}

func (s *createKnowledgeTaskEnqueuerStub) PrepareDocumentWorkflow(
	_ context.Context,
	task *asynq.Task,
	opts ...asynq.Option,
) (*documentqueue.Workflow, bool, error) {
	s.prepareCalls++
	if s.events != nil {
		*s.events = append(*s.events, "prepare")
	}
	s.payloads = append(s.payloads, append([]byte(nil), task.Payload()...))
	s.taskTypes = append(s.taskTypes, task.Type())
	queue := types.QueueDefault
	for _, opt := range opts {
		if opt.Type() == asynq.QueueOpt {
			if q, ok := opt.Value().(string); ok && q != "" {
				queue = q
			}
		}
	}
	s.queues = append(s.queues, queue)
	var identity struct {
		TenantID             uint64 `json:"tenant_id"`
		KnowledgeID          string `json:"knowledge_id"`
		KnowledgeBaseID      string `json:"knowledge_base_id"`
		ProcessingGeneration string `json:"processing_generation"`
	}
	if err := json.Unmarshal(task.Payload(), &identity); err != nil {
		return nil, false, err
	}
	return &documentqueue.Workflow{
		ID: "00000000-0000-0000-0000-000000000001", TenantID: identity.TenantID,
		KnowledgeID: identity.KnowledgeID, KnowledgeBaseID: identity.KnowledgeBaseID,
		ProcessingGeneration: identity.ProcessingGeneration,
		TaskType:             task.Type(), Payload: append([]byte(nil), task.Payload()...),
		State: documentqueue.StatePreparing,
	}, true, nil
}

func (s *createKnowledgeTaskEnqueuerStub) AbortDocumentWorkflow(
	context.Context, documentqueue.WorkflowBinding, string,
) error {
	s.abortCalls++
	if s.events != nil {
		*s.events = append(*s.events, "abort")
	}
	return nil
}

func (s *createKnowledgeTaskEnqueuerStub) ResumeDocumentWorkflow(
	_ context.Context, _ documentqueue.WorkflowBinding,
) (*asynq.TaskInfo, error) {
	s.calls++
	if s.events != nil {
		*s.events = append(*s.events, "resume")
	}
	if s.err != nil {
		return nil, s.err
	}
	return &asynq.TaskInfo{ID: "task-1", Queue: types.QueueDocument}, nil
}

func TestCreateKnowledgeFromFileDoesNotPersistWhenStorageSaveFails(t *testing.T) {
	t.Parallel()

	repo := &createKnowledgeFileRepoStub{}
	fileSvc := &createKnowledgeFileServiceStub{saveErr: errors.New("storage unavailable")}
	svc := &knowledgeService{
		repo:      repo,
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}},
		fileSvc:   fileSvc,
	}
	configureCreateKnowledgeAuxiliary(t, svc, fileSvc)

	knowledge, err := svc.CreateKnowledgeFromFile(
		newCreateKnowledgeFileContext(),
		"kb-1",
		newMultipartFileHeader(t, "doc.txt", "hello"),
		nil,
		nil,
		"",
		nil,
		"",
		nil,
	)

	require.Error(t, err)
	require.Nil(t, knowledge)
	require.Equal(t, 1, fileSvc.saveCalls)
	require.Zero(t, repo.createCalls)
}

func TestCreateKnowledgeFromFilePersistsStoredFilePathOnCreate(t *testing.T) {
	t.Parallel()

	repo := &createKnowledgeFileRepoStub{}
	fileSvc := &createKnowledgeFileServiceStub{}
	task := &createKnowledgeTaskEnqueuerStub{}
	svc := &knowledgeService{
		repo:      repo,
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}},
		fileSvc:   fileSvc,
		task:      task,
	}
	configureCreateKnowledgeAuxiliary(t, svc, fileSvc)

	knowledge, err := svc.CreateKnowledgeFromFile(
		newCreateKnowledgeFileContext(),
		"kb-1",
		newMultipartFileHeader(t, "doc.txt", "hello"),
		nil,
		nil,
		"",
		nil,
		"",
		nil,
	)

	require.NoError(t, err)
	require.NotNil(t, knowledge)
	require.Equal(t, 1, fileSvc.saveCalls)
	require.NotEmpty(t, fileSvc.savedWithKnowledgeID)
	require.Equal(t, fileSvc.savedWithKnowledgeID, knowledge.ID)
	require.Equal(t, 1, repo.createCalls)
	require.NotNil(t, repo.createdKnowledge)
	require.Equal(t, "stored/"+knowledge.ID, repo.createdKnowledge.FilePath)
	require.Equal(t, 1, task.calls)
}

func TestCreateKnowledgeFromFileKeepsHeavyFileInDocumentWorkflowQueue(t *testing.T) {
	t.Parallel()

	repo := &createKnowledgeFileRepoStub{}
	fileSvc := &createKnowledgeFileServiceStub{}
	task := &createKnowledgeTaskEnqueuerStub{}
	svc := &knowledgeService{
		repo:      repo,
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}},
		fileSvc:   fileSvc,
		task:      task,
	}
	configureCreateKnowledgeAuxiliary(t, svc, fileSvc)

	file := newMultipartFileHeader(t, "large.pdf", strings.Repeat("x", 10*1024*1024+1))
	require.Greater(t, file.Size, int64(10*1024*1024))
	require.True(t, fileguard.AnalyzeMultipartFile(file, "large.pdf").IsHeavy())

	knowledge, err := svc.CreateKnowledgeFromFile(
		newCreateKnowledgeFileContext(),
		"kb-1",
		file,
		nil,
		nil,
		"",
		nil,
		"",
		nil,
	)

	require.NoError(t, err)
	require.NotNil(t, knowledge)
	require.Equal(t, []string{types.QueueDocument}, task.queues)
}

func TestCreateKnowledgeFromFileResumeFailureLeavesDurableBoundGeneration(t *testing.T) {
	t.Parallel()

	repo := &createKnowledgeFileRepoStub{}
	fileSvc := &createKnowledgeFileServiceStub{}
	queueErr := errors.New("redis unavailable")
	task := &createKnowledgeTaskEnqueuerStub{err: queueErr}
	svc := &knowledgeService{
		repo:      repo,
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}},
		fileSvc:   fileSvc,
		task:      task,
	}
	configureCreateKnowledgeAuxiliary(t, svc, fileSvc)

	knowledge, err := svc.CreateKnowledgeFromFile(
		newCreateKnowledgeFileContext(),
		"kb-1",
		newMultipartFileHeader(t, "doc.txt", "hello"),
		nil,
		nil,
		"",
		nil,
		"",
		nil,
	)

	require.NoError(t, err)
	require.NotNil(t, knowledge)
	require.Equal(t, 1, task.calls)
	require.Zero(t, repo.processingCAS)
	require.Equal(t, types.ParseStatusPending, repo.createdKnowledge.ParseStatus)
	require.NotEmpty(t, repo.createdKnowledge.ProcessingOwner)
	require.NotEmpty(t, repo.createdKnowledge.ProcessingGeneration)
	require.NotEmpty(t, repo.createdKnowledge.ProcessingWorkflowID)
	require.Empty(t, repo.createdKnowledge.ErrorMessage)
}

func TestCreateKnowledgeFromFileDeletesStoredFileWhenCreateFails(t *testing.T) {
	t.Parallel()

	repo := &createKnowledgeFileRepoStub{createErr: errors.New("database unavailable")}
	fileSvc := &createKnowledgeFileServiceStub{}
	task := &createKnowledgeTaskEnqueuerStub{}
	svc := &knowledgeService{
		repo:      repo,
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}},
		fileSvc:   fileSvc,
		task:      task,
	}
	configureCreateKnowledgeAuxiliary(t, svc, fileSvc)

	knowledge, err := svc.CreateKnowledgeFromFile(
		newCreateKnowledgeFileContext(),
		"kb-1",
		newMultipartFileHeader(t, "doc.txt", "hello"),
		nil,
		nil,
		"",
		nil,
		"",
		nil,
	)

	require.ErrorContains(t, err, "database unavailable")
	require.Nil(t, knowledge)
	require.Equal(t, 1, fileSvc.saveCalls)
	require.Equal(t, 1, repo.createCalls)
	require.Equal(t, 1, fileSvc.deleteCalls)
	require.Equal(t, "stored/"+fileSvc.savedWithKnowledgeID, fileSvc.deletedPath)
	require.Equal(t, 1, task.abortCalls)
}

func TestCreateKnowledgeFromFile_PersistsProcessOverrides(t *testing.T) {
	t.Parallel()

	repo := &createKnowledgeFileRepoStub{}
	fileSvc := &createKnowledgeFileServiceStub{}
	task := &createKnowledgeTaskEnqueuerStub{}
	svc := &knowledgeService{
		repo:      repo,
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}},
		fileSvc:   fileSvc,
		task:      task,
	}
	configureCreateKnowledgeAuxiliary(t, svc, fileSvc)

	chunkSize := 512
	overrides := &types.KnowledgeProcessOverrides{
		ChunkingConfig: &types.ChunkingConfig{ChunkSize: chunkSize},
	}

	knowledge, err := svc.CreateKnowledgeFromFile(
		newCreateKnowledgeFileContext(),
		"kb-1",
		newMultipartFileHeader(t, "doc.txt", "hello"),
		map[string]string{"source": "test"},
		nil,
		"",
		nil,
		"",
		overrides,
	)

	require.NoError(t, err)
	require.NotNil(t, knowledge)
	require.Equal(t, 1, repo.createCalls)
	require.NotNil(t, repo.createdKnowledge)

	parsed, err := repo.createdKnowledge.ProcessOverrides()
	require.NoError(t, err)
	require.NotNil(t, parsed)
	require.NotNil(t, parsed.ChunkingConfig)
	require.Equal(t, chunkSize, parsed.ChunkingConfig.ChunkSize)

	metadataMap, err := repo.createdKnowledge.Metadata.Map()
	require.NoError(t, err)
	require.Equal(t, "test", metadataMap["source"])
}

func newCreateKnowledgeFileContext() context.Context {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, &types.Tenant{})
	return ctx
}

func newMultipartFileHeader(t *testing.T, filename string, content string) *multipart.FileHeader {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest("POST", "/", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	require.NoError(t, req.ParseMultipartForm(1024))
	fh := req.MultipartForm.File["file"][0]
	fh.Size = int64(len(content))
	return fh
}
