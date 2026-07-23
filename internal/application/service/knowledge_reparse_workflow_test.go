package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

type reparseWorkflowEnqueuerStub struct {
	genericEnqueueCalls int
	prepareCalls        int
	commitCalls         int
	resumeCalls         int
	abortCalls          int
	commitErr           error
	payloads            [][]byte
	lastBinding         documentqueue.WorkflowBinding
}

func (s *reparseWorkflowEnqueuerStub) Enqueue(
	*asynq.Task, ...asynq.Option,
) (*asynq.TaskInfo, error) {
	s.genericEnqueueCalls++
	return nil, errors.New("generic root enqueue must not be used by reparse")
}

func (s *reparseWorkflowEnqueuerStub) PrepareDocumentWorkflow(
	_ context.Context, task *asynq.Task, _ ...asynq.Option,
) (*documentqueue.Workflow, bool, error) {
	s.prepareCalls++
	s.payloads = append(s.payloads, append([]byte(nil), task.Payload()...))
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
		ID:       "00000000-0000-0000-0000-000000000099",
		TenantID: identity.TenantID, KnowledgeID: identity.KnowledgeID,
		KnowledgeBaseID:      identity.KnowledgeBaseID,
		ProcessingGeneration: identity.ProcessingGeneration,
		TaskType:             task.Type(), Payload: append([]byte(nil), task.Payload()...),
		State: documentqueue.StatePreparing,
	}, s.prepareCalls == 1, nil
}

func (s *reparseWorkflowEnqueuerStub) AbortDocumentWorkflow(
	context.Context, documentqueue.WorkflowBinding, string,
) error {
	s.abortCalls++
	return nil
}

func (s *reparseWorkflowEnqueuerStub) ResumeDocumentWorkflow(
	_ context.Context, binding documentqueue.WorkflowBinding,
) (*asynq.TaskInfo, error) {
	s.resumeCalls++
	s.lastBinding = binding
	return &asynq.TaskInfo{ID: "delivery", Queue: types.QueueDocument}, nil
}

func (s *reparseWorkflowEnqueuerStub) CommitPreparedReparse(
	_ context.Context,
	binding documentqueue.WorkflowBinding,
	_ documentqueue.ReparsePendingTransition,
) error {
	s.commitCalls++
	s.lastBinding = binding
	return s.commitErr
}

func reparseWorkflowKnowledge() *types.Knowledge {
	knowledge := &types.Knowledge{
		ID: "knowledge-reparse", TenantID: 7, KnowledgeBaseID: "kb-reparse",
		Type: "url", Source: "https://example.test/document",
		ParseStatus:          types.ParseStatusProcessing,
		ProcessingGeneration: "stable-generation",
	}
	knowledge.ProcessingOwner = processownership.DocumentOwner(knowledge.ID, knowledge.ProcessingGeneration)
	return knowledge
}

func TestReparseProductionCompositionPreparesCommitsAndResumesWithoutGenericEnqueue(t *testing.T) {
	task := &reparseWorkflowEnqueuerStub{}
	service := &knowledgeService{task: task}
	knowledge := reparseWorkflowKnowledge()
	kb := &types.KnowledgeBase{ID: knowledge.KnowledgeBaseID, EmbeddingModelID: "embedding-1"}
	tracing := &types.TracingContext{
		LangfuseTraceID: "original-trace", LangfuseParentObservationID: "original-parent",
		LangfuseUserID: "tenant:7", LangfuseSessionID: "request-stable",
	}

	prepared, err := service.prepareReparseDocumentWorkflow(
		context.Background(), knowledge, kb, types.EffectiveProcessConfig{}, 2, tracing,
	)
	require.NoError(t, err)
	require.Equal(t, 1, task.prepareCalls)
	require.Zero(t, task.genericEnqueueCalls)

	now := time.Now()
	require.NoError(t, service.commitPreparedReparseWorkflow(
		context.Background(), prepared, knowledge, kb.EmbeddingModelID,
		batchReparseMarker(batchReparseReady, knowledge.ProcessingGeneration, 2), now,
	))
	require.Equal(t, 1, task.commitCalls)
	require.NotEmpty(t, knowledge.ProcessingWorkflowID)
	require.Equal(t, knowledge.ProcessingWorkflowID, task.lastBinding.WorkflowID)

	_, err = service.resumeBatchReparseSubmission(context.Background(), knowledge, kb, 2)
	require.NoError(t, err)
	require.Equal(t, 1, task.resumeCalls)
	require.Zero(t, task.genericEnqueueCalls)
}

func TestStableBatchReparseRootPlanIgnoresRetryObservationContext(t *testing.T) {
	task := &reparseWorkflowEnqueuerStub{}
	service := &knowledgeService{task: task}
	knowledge := reparseWorkflowKnowledge()
	kb := &types.KnowledgeBase{ID: knowledge.KnowledgeBaseID}
	stable := &types.TracingContext{
		LangfuseTraceID: "parent-trace", LangfuseParentObservationID: "parent-observation",
		LangfuseUserID: "tenant:7", LangfuseSessionID: "request-original",
	}

	for _, requestID := range []string{"retry-observation-a", "retry-observation-b"} {
		ctx := context.WithValue(context.Background(), types.RequestIDContextKey, requestID)
		_, err := service.prepareReparseDocumentWorkflow(
			ctx, knowledge, kb, types.EffectiveProcessConfig{}, 4, stable,
		)
		require.NoError(t, err)
	}
	require.Len(t, task.payloads, 2)
	require.Equal(t, task.payloads[0], task.payloads[1], "same stable generation must produce one immutable plan")

	var payload types.DocumentProcessPayload
	require.NoError(t, json.Unmarshal(task.payloads[0], &payload))
	require.Equal(t, *stable, payload.TracingContext)
}

func TestStableBatchCommitFailurePreservesOnlyPreparedWorkflow(t *testing.T) {
	task := &reparseWorkflowEnqueuerStub{}
	service := &knowledgeService{task: task}
	prepared := &preparedDocumentWorkflow{binding: documentqueue.WorkflowBinding{
		WorkflowID: "workflow-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-1",
		ProcessingOwner: processownership.DocumentOwner("knowledge-1", "generation-1"),
	}}

	service.handleUncommittedReparseWorkflow(context.Background(), prepared, true)
	require.Zero(t, task.abortCalls, "stable generation must retry the same Preparing plan")
	service.handleUncommittedReparseWorkflow(context.Background(), prepared, false)
	require.Equal(t, 1, task.abortCalls, "standalone generation may discard an unbound plan")
}

func TestStableManualBatchReparseRootPlanIsByteStable(t *testing.T) {
	knowledge := reparseWorkflowKnowledge()
	knowledge.Type = types.KnowledgeTypeManual
	knowledge.Source = types.KnowledgeTypeManual
	stable := &types.TracingContext{
		LangfuseTraceID: "parent-trace", LangfuseParentObservationID: "parent-observation",
		LangfuseSessionID: "request-original",
	}
	var payloads [][]byte
	for _, requestID := range []string{"retry-a", "retry-b"} {
		ctx := context.WithValue(context.Background(), types.RequestIDContextKey, requestID)
		task, _, err := buildManualProcessingTaskWithTracing(
			ctx, knowledge, "# immutable manual content", true, 6, stable,
		)
		require.NoError(t, err)
		payloads = append(payloads, task.Payload())
	}
	require.Equal(t, payloads[0], payloads[1])
	var payload types.ManualProcessPayload
	require.NoError(t, json.Unmarshal(payloads[0], &payload))
	require.Equal(t, "request-original", payload.RequestId)
	require.Equal(t, *stable, payload.TracingContext)
}

func TestReparseDocumentWorkflowBuilderCoversEveryDocumentSource(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*types.Knowledge)
		assert    func(*testing.T, types.DocumentProcessPayload)
	}{
		{
			name: "stored file",
			configure: func(knowledge *types.Knowledge) {
				knowledge.Type = "file"
				knowledge.FilePath = "local://stable/document.txt"
				knowledge.FileName = "document.txt"
			},
			assert: func(t *testing.T, payload types.DocumentProcessPayload) {
				require.Equal(t, "local://stable/document.txt", payload.FilePath)
				require.Equal(t, "document.txt", payload.FileName)
				require.Equal(t, "txt", payload.FileType)
			},
		},
		{
			name: "file URL",
			configure: func(knowledge *types.Knowledge) {
				knowledge.Type = "file_url"
				knowledge.Source = "https://example.test/document.pdf"
				knowledge.FileName = "document.pdf"
				knowledge.FileType = "pdf"
			},
			assert: func(t *testing.T, payload types.DocumentProcessPayload) {
				require.Equal(t, "https://example.test/document.pdf", payload.FileURL)
			},
		},
		{
			name: "web URL",
			configure: func(knowledge *types.Knowledge) {
				knowledge.Type = "url"
				knowledge.Source = "https://example.test/article"
			},
			assert: func(t *testing.T, payload types.DocumentProcessPayload) {
				require.Equal(t, "https://example.test/article", payload.URL)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := &reparseWorkflowEnqueuerStub{}
			service := &knowledgeService{task: task}
			knowledge := reparseWorkflowKnowledge()
			knowledge.Type, knowledge.Source = "", ""
			test.configure(knowledge)
			_, err := service.prepareReparseDocumentWorkflow(
				context.Background(), knowledge,
				&types.KnowledgeBase{ID: knowledge.KnowledgeBaseID},
				types.EffectiveProcessConfig{}, 1, &types.TracingContext{},
			)
			require.NoError(t, err)
			require.Equal(t, 1, task.prepareCalls)
			require.Zero(t, task.genericEnqueueCalls)
			var payload types.DocumentProcessPayload
			require.NoError(t, json.Unmarshal(task.payloads[0], &payload))
			test.assert(t, payload)
		})
	}
}
