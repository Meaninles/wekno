package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func newWorkflowProducerTestService(
	repo *createKnowledgeFileRepoStub,
	task *createKnowledgeTaskEnqueuerStub,
) *knowledgeService {
	return &knowledgeService{
		repo: repo,
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{
			ID: "kb-1",
			QuestionGenerationConfig: &types.QuestionGenerationConfig{
				Enabled: true, QuestionCount: 7,
			},
		}},
		fileSvc: &createKnowledgeFileServiceStub{},
		task:    task,
	}
}

func TestAsyncPassagePreparesExactPlanBeforeKnowledgeCommit(t *testing.T) {
	events := []string{}
	repo := &createKnowledgeFileRepoStub{events: &events}
	task := &createKnowledgeTaskEnqueuerStub{events: &events}
	svc := newWorkflowProducerTestService(repo, task)

	knowledge, err := svc.CreateKnowledgeFromPassage(
		newCreateKnowledgeFileContext(), "kb-1", []string{"first", "second"}, "api",
	)
	require.NoError(t, err)
	require.Equal(t, []string{"prepare", "create", "resume"}, events)
	require.Equal(t, 1, task.prepareCalls)
	require.NotEmpty(t, repo.createdKnowledge.ProcessingWorkflowID)
	require.Equal(t, repo.createdKnowledge.ProcessingWorkflowID, knowledge.ProcessingWorkflowID)

	var payload types.DocumentProcessPayload
	require.NoError(t, json.Unmarshal(task.payloads[0], &payload))
	require.Equal(t, []string{"first", "second"}, payload.Passages)
	require.Equal(t, knowledge.ProcessingGeneration, payload.ProcessingGeneration)
	require.Equal(t, knowledge.ProcessingOwner, payload.ProcessingOwner)
	require.True(t, payload.EnableQuestionGeneration)
	require.Equal(t, 7, payload.QuestionCount)
}

func TestSynchronousPassageDoesNotCreateDocumentWorkflow(t *testing.T) {
	repo := &createKnowledgeFileRepoStub{createErr: errors.New("stop before synchronous processing")}
	task := &createKnowledgeTaskEnqueuerStub{}
	svc := newWorkflowProducerTestService(repo, task)

	_, err := svc.CreateKnowledgeFromPassageSync(
		newCreateKnowledgeFileContext(), "kb-1", []string{"sync"}, "api",
	)
	require.ErrorContains(t, err, "stop before synchronous processing")
	require.Zero(t, task.prepareCalls)
	require.Zero(t, task.calls)
	require.Empty(t, repo.createdKnowledge.ProcessingWorkflowID)
}

func TestURLAndFileURLPrepareBeforeAtomicCreate(t *testing.T) {
	tests := []struct {
		name            string
		url             string
		fileName        string
		fileType        string
		expectedURL     string
		expectedFileURL string
	}{
		{name: "html URL", url: "https://example.com/article", expectedURL: "https://example.com/article"},
		{name: "file URL", url: "https://example.com/manual.pdf", fileName: "manual.pdf", fileType: "pdf", expectedFileURL: "https://example.com/manual.pdf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := []string{}
			repo := &createKnowledgeFileRepoStub{events: &events}
			task := &createKnowledgeTaskEnqueuerStub{events: &events}
			svc := newWorkflowProducerTestService(repo, task)

			knowledge, err := svc.CreateKnowledgeFromURL(
				newCreateKnowledgeFileContext(), "kb-1", tt.url, tt.fileName, tt.fileType,
				nil, "title", nil, "api", nil,
			)
			require.NoError(t, err)
			require.Equal(t, []string{"prepare", "create", "resume"}, events)
			require.NotEmpty(t, knowledge.ProcessingWorkflowID)

			var payload types.DocumentProcessPayload
			require.NoError(t, json.Unmarshal(task.payloads[0], &payload))
			require.Equal(t, tt.expectedURL, payload.URL)
			require.Equal(t, tt.expectedFileURL, payload.FileURL)
			require.Equal(t, knowledge.ProcessingGeneration, payload.ProcessingGeneration)
			require.Equal(t, knowledge.ProcessingOwner, payload.ProcessingOwner)
		})
	}
}

func TestManualPublishBindsExactContentBeforeCreateAndDraftDoesNotQueue(t *testing.T) {
	t.Run("publish", func(t *testing.T) {
		events := []string{}
		repo := &createKnowledgeFileRepoStub{events: &events}
		task := &createKnowledgeTaskEnqueuerStub{events: &events}
		svc := newWorkflowProducerTestService(repo, task)

		knowledge, err := svc.CreateKnowledgeFromManual(
			newCreateKnowledgeFileContext(), "kb-1", &types.ManualKnowledgePayload{
				Title: "manual", Content: "# exact content", Status: types.ManualKnowledgeStatusPublish,
			}, "api",
		)
		require.NoError(t, err)
		require.Equal(t, []string{"prepare", "create", "resume"}, events)
		require.NotEmpty(t, knowledge.ProcessingWorkflowID)

		var payload types.ManualProcessPayload
		require.NoError(t, json.Unmarshal(task.payloads[0], &payload))
		require.Equal(t, "# exact content", payload.Content)
		require.False(t, payload.NeedCleanup)
		require.Equal(t, knowledge.ProcessingGeneration, payload.ProcessingGeneration)
	})

	t.Run("draft", func(t *testing.T) {
		events := []string{}
		repo := &createKnowledgeFileRepoStub{events: &events}
		task := &createKnowledgeTaskEnqueuerStub{events: &events}
		svc := newWorkflowProducerTestService(repo, task)

		knowledge, err := svc.CreateKnowledgeFromManual(
			newCreateKnowledgeFileContext(), "kb-1", &types.ManualKnowledgePayload{
				Title: "draft", Content: "draft content", Status: types.ManualKnowledgeStatusDraft,
			}, "api",
		)
		require.NoError(t, err)
		require.Equal(t, []string{"create"}, events)
		require.Zero(t, task.prepareCalls)
		require.Empty(t, knowledge.ProcessingWorkflowID)
	})
}

func TestManualUpdateCommitsNewWorkflowIDWithPendingGeneration(t *testing.T) {
	old := &types.Knowledge{
		ID: "knowledge-1", TenantID: 1, KnowledgeBaseID: "kb-1",
		Type: types.KnowledgeTypeManual, Source: types.KnowledgeTypeManual,
		ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "old-generation",
		ProcessingWorkflowID: "old-workflow", Title: "old", FileName: "old.md",
	}
	require.NoError(t, old.SetManualMetadata(types.NewManualKnowledgeMetadata("old", types.ManualKnowledgeStatusPublish, 1)))
	events := []string{}
	repo := &createKnowledgeFileRepoStub{createdKnowledge: old, events: &events}
	task := &createKnowledgeTaskEnqueuerStub{events: &events, err: errors.New("redis unavailable")}
	svc := newWorkflowProducerTestService(repo, task)

	updated, err := svc.UpdateManualKnowledge(
		newCreateKnowledgeFileContext(), old.ID, &types.ManualKnowledgePayload{
			Title: "new", Content: "new exact content", Status: types.ManualKnowledgeStatusPublish,
		},
	)
	require.NoError(t, err, "a committed binding remains accepted when immediate resume fails")
	require.Equal(t, 1, task.prepareCalls)
	require.Equal(t, 1, task.calls)
	require.Equal(t, 1, repo.processingCAS)
	require.NotEqual(t, "old-workflow", updated.ProcessingWorkflowID)
	require.Equal(t, updated.ProcessingWorkflowID, repo.lastCASValues["processing_workflow_id"])
	require.Equal(t, types.ParseStatusPending, repo.lastCASValues["parse_status"])

	var taskPayload types.ManualProcessPayload
	require.NoError(t, json.Unmarshal(task.payloads[0], &taskPayload))
	require.Equal(t, "new exact content", taskPayload.Content)
	require.True(t, taskPayload.NeedCleanup)
	require.Equal(t, updated.ProcessingGeneration, taskPayload.ProcessingGeneration)
}

func TestPreparedProducerFailureAbortsOnlyBeforeBusinessBinding(t *testing.T) {
	t.Run("async passage create rollback", func(t *testing.T) {
		repo := &createKnowledgeFileRepoStub{createErr: errors.New("database unavailable")}
		task := &createKnowledgeTaskEnqueuerStub{}
		svc := newWorkflowProducerTestService(repo, task)

		_, err := svc.CreateKnowledgeFromPassage(
			newCreateKnowledgeFileContext(), "kb-1", []string{"will roll back"}, "api",
		)
		require.ErrorContains(t, err, "database unavailable")
		require.Equal(t, 1, task.prepareCalls)
		require.Equal(t, 1, task.abortCalls)
		require.Zero(t, task.calls)
	})

	t.Run("manual publish CAS conflict", func(t *testing.T) {
		old := &types.Knowledge{
			ID: "knowledge-conflict", TenantID: 1, KnowledgeBaseID: "kb-1",
			Type: types.KnowledgeTypeManual, Source: types.KnowledgeTypeManual,
			ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "old-generation",
			Title: "old", FileName: "old.md",
		}
		require.NoError(t, old.SetManualMetadata(types.NewManualKnowledgeMetadata("old", types.ManualKnowledgeStatusPublish, 1)))
		repo := &createKnowledgeFileRepoStub{createdKnowledge: old, forceCASConflict: true}
		task := &createKnowledgeTaskEnqueuerStub{}
		svc := newWorkflowProducerTestService(repo, task)

		_, err := svc.UpdateManualKnowledge(
			newCreateKnowledgeFileContext(), old.ID, &types.ManualKnowledgePayload{
				Title: "new", Content: "new", Status: types.ManualKnowledgeStatusPublish,
			},
		)
		require.Error(t, err)
		require.Equal(t, 1, task.prepareCalls)
		require.Equal(t, 1, task.abortCalls)
		require.Zero(t, task.calls)
	})
}
