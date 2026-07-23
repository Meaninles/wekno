package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
)

type deadLetterFenceRow struct {
	tenantID        uint64
	knowledgeID     string
	knowledgeBaseID string
	generation      string
	owner           string
	status          string
	processed       bool
	pendingSubtasks int
	hasFanout       bool
	errorMessage    string
	summaryStatus   string
}

type deadLetterFenceFake struct {
	mu               sync.Mutex
	row              deadLetterFenceRow
	calls            int
	documentCalls    int
	postProcessCalls int
}

func (f *deadLetterFenceFake) CompletePostProcessDeadLetterGeneration(
	_ context.Context,
	tenantID uint64,
	id string,
	knowledgeBaseID string,
	expectedGeneration string,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.postProcessCalls++
	if f.row.tenantID != tenantID || f.row.knowledgeID != id ||
		f.row.knowledgeBaseID != knowledgeBaseID || f.row.generation != expectedGeneration ||
		!f.row.processed ||
		(f.row.status != types.ParseStatusProcessing && f.row.status != types.ParseStatusFinalizing) {
		return false, nil
	}
	f.row.status = types.ParseStatusCompleted
	f.row.pendingSubtasks = 0
	f.row.owner = ""
	f.row.hasFanout = false
	f.row.errorMessage = ""
	if f.row.summaryStatus == types.SummaryStatusPending || f.row.summaryStatus == types.SummaryStatusProcessing {
		f.row.summaryStatus = types.SummaryStatusFailed
	}
	return true, nil
}

func (f *deadLetterFenceFake) FailDocumentProcessingGeneration(
	_ context.Context,
	tenantID uint64,
	id string,
	knowledgeBaseID string,
	expectedGeneration string,
	expectedOwner string,
	values map[string]interface{},
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.documentCalls++
	if f.row.tenantID != tenantID || f.row.knowledgeID != id ||
		f.row.knowledgeBaseID != knowledgeBaseID || f.row.generation != expectedGeneration ||
		f.row.owner != expectedOwner || f.row.processed ||
		(f.row.status != types.ParseStatusPending && f.row.status != types.ParseStatusProcessing) {
		return false, nil
	}
	if err := applyDeadLetterFailureValues(&f.row, values); err != nil {
		return false, err
	}
	return true, nil
}

func applyDeadLetterFailureValues(row *deadLetterFenceRow, values map[string]interface{}) error {
	status, ok := values["parse_status"].(string)
	if !ok {
		return errors.New("parse_status update is required")
	}
	errorMessage, ok := values["error_message"].(string)
	if !ok {
		return errors.New("error_message update is required")
	}
	pendingSubtasks, ok := values["pending_subtasks_count"].(int)
	if !ok {
		return errors.New("pending_subtasks_count update is required")
	}
	owner, ok := values["processing_owner"].(string)
	if !ok {
		return errors.New("processing_owner update is required")
	}
	fanout, ok := values["processing_fanout"]
	if !ok || fanout != nil {
		return errors.New("processing_fanout must be cleared")
	}
	row.status = status
	row.errorMessage = errorMessage
	row.pendingSubtasks = pendingSubtasks
	row.owner = owner
	row.hasFanout = false
	return nil
}

func deadLetterTaskForTest(t *testing.T, taskType string, payload map[string]interface{}) *asynq.Task {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return asynq.NewTask(taskType, raw)
}

func TestDeadLetterKnowledgeStatusFailerUsesCompleteGenerationFence(t *testing.T) {
	tests := []struct {
		name     string
		taskType string
		status   string
		payload  map[string]interface{}
	}{
		{
			name:     "document pending",
			taskType: types.TypeDocumentProcess,
			status:   types.ParseStatusPending,
			payload: map[string]interface{}{
				"processing_generation": "generation-1",
				"processing_owner":      "owner-1",
			},
		},
		{
			name:     "postprocess processing after core commit",
			taskType: types.TypeKnowledgePostProcess,
			status:   types.ParseStatusProcessing,
			payload: map[string]interface{}{
				"processing_generation": "generation-1",
			},
		},
		{
			name:     "postprocess finalizing",
			taskType: types.TypeKnowledgePostProcess,
			status:   types.ParseStatusFinalizing,
			payload: map[string]interface{}{
				"processing_generation": "generation-1",
			},
		},
		{
			name:     "manual explicit processing identity",
			taskType: types.TypeManualProcess,
			status:   types.ParseStatusProcessing,
			payload: map[string]interface{}{
				"processing_generation": "generation-1",
				"processing_owner":      "owner-1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isPostProcess := tt.taskType == types.TypeKnowledgePostProcess
			repo := &deadLetterFenceFake{row: deadLetterFenceRow{
				tenantID:        42,
				knowledgeID:     "knowledge-1",
				knowledgeBaseID: "kb-1",
				generation:      "generation-1",
				owner:           "owner-1",
				status:          tt.status,
				processed:       isPostProcess,
				pendingSubtasks: 7,
				hasFanout:       true,
				summaryStatus:   types.SummaryStatusPending,
			}}
			payload := map[string]interface{}{
				"tenant_id":         42,
				"knowledge_id":      "knowledge-1",
				"knowledge_base_id": "kb-1",
			}
			for key, value := range tt.payload {
				payload[key] = value
			}

			callback := newDeadLetterKnowledgeStatusFailer(repo, repo, nil)
			callback(context.Background(), deadLetterTaskForTest(t, tt.taskType, payload), errors.New("retry budget exhausted"))

			repo.mu.Lock()
			defer repo.mu.Unlock()
			if repo.calls != 1 {
				t.Fatalf("CAS calls = %d, want 1", repo.calls)
			}
			wantStatus := types.ParseStatusFailed
			if isPostProcess {
				wantStatus = types.ParseStatusCompleted
			}
			if repo.row.status != wantStatus {
				t.Fatalf("status = %q, want %q", repo.row.status, wantStatus)
			}
			if isPostProcess {
				if repo.row.errorMessage != "" || repo.row.summaryStatus != types.SummaryStatusFailed {
					t.Fatalf("postprocess degradation incomplete: error=%q summary=%q",
						repo.row.errorMessage, repo.row.summaryStatus)
				}
			} else if !strings.Contains(repo.row.errorMessage, "retry budget exhausted") {
				t.Fatalf("error_message = %q, want task failure", repo.row.errorMessage)
			}
			if repo.row.pendingSubtasks != 0 || repo.row.owner != "" || repo.row.hasFanout {
				t.Fatalf("terminal cleanup incomplete: pending=%d owner=%q fanout=%v",
					repo.row.pendingSubtasks, repo.row.owner, repo.row.hasFanout)
			}
			if tt.taskType != types.TypeKnowledgePostProcess {
				if repo.documentCalls != 1 || repo.postProcessCalls != 0 {
					t.Fatalf("document/postprocess calls = %d/%d, want 1/0", repo.documentCalls, repo.postProcessCalls)
				}
				return
			}
			if repo.documentCalls != 0 || repo.postProcessCalls != 1 {
				t.Fatalf("document/postprocess calls = %d/%d, want 0/1", repo.documentCalls, repo.postProcessCalls)
			}
		})
	}
}

func TestDeadLetterKnowledgeStatusFailerCannotRevertNewOrCompletedGeneration(t *testing.T) {
	tests := []struct {
		name       string
		generation string
		status     string
	}{
		{name: "new generation is processing", generation: "generation-2", status: types.ParseStatusProcessing},
		{name: "new generation completed", generation: "generation-2", status: types.ParseStatusCompleted},
		{name: "same generation completed", generation: "generation-1", status: types.ParseStatusCompleted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &deadLetterFenceFake{row: deadLetterFenceRow{
				tenantID:        42,
				knowledgeID:     "knowledge-1",
				knowledgeBaseID: "kb-1",
				generation:      tt.generation,
				owner:           "owner-1",
				status:          tt.status,
				pendingSubtasks: 5,
				hasFanout:       true,
				errorMessage:    "current generation state",
			}}
			callback := newDeadLetterKnowledgeStatusFailer(repo, repo, nil)
			callback(context.Background(), deadLetterTaskForTest(t, types.TypeDocumentProcess, map[string]interface{}{
				"tenant_id":             42,
				"knowledge_id":          "knowledge-1",
				"knowledge_base_id":     "kb-1",
				"processing_generation": "generation-1",
				"processing_owner":      "owner-1",
			}), errors.New("stale worker exhausted"))

			repo.mu.Lock()
			defer repo.mu.Unlock()
			if repo.calls != 1 {
				t.Fatalf("CAS calls = %d, want 1", repo.calls)
			}
			if repo.row.status != tt.status {
				t.Fatalf("status = %q, want unchanged %q", repo.row.status, tt.status)
			}
			if repo.row.errorMessage != "current generation state" {
				t.Fatalf("error_message = %q, want unchanged", repo.row.errorMessage)
			}
			if repo.row.pendingSubtasks != 5 || repo.row.owner != "owner-1" || !repo.row.hasFanout {
				t.Fatalf("stale dead letter changed cleanup state: pending=%d owner=%q fanout=%v",
					repo.row.pendingSubtasks, repo.row.owner, repo.row.hasFanout)
			}
		})
	}
}

func TestDeadLetterDocumentStatusFailerRequiresLiveOwnerAndUncommittedCore(t *testing.T) {
	tests := []struct {
		name      string
		rowOwner  string
		processed bool
		status    string
	}{
		{
			name:      "core commit consumed owner and set processed at",
			rowOwner:  "",
			processed: true,
			status:    types.ParseStatusProcessing,
		},
		{
			name:      "processed at alone invalidates delayed dead letter",
			rowOwner:  "owner-1",
			processed: true,
			status:    types.ParseStatusProcessing,
		},
		{
			name:     "same generation belongs to different owner",
			rowOwner: "owner-2",
			status:   types.ParseStatusProcessing,
		},
		{
			name:     "pending belongs to different planned owner",
			rowOwner: "owner-2",
			status:   types.ParseStatusPending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &deadLetterFenceFake{row: deadLetterFenceRow{
				tenantID:        42,
				knowledgeID:     "knowledge-1",
				knowledgeBaseID: "kb-1",
				generation:      "generation-1",
				owner:           tt.rowOwner,
				status:          tt.status,
				processed:       tt.processed,
				pendingSubtasks: 5,
				hasFanout:       true,
				errorMessage:    "current state",
			}}
			callback := newDeadLetterKnowledgeStatusFailer(repo, repo, nil)
			callback(context.Background(), deadLetterTaskForTest(t, types.TypeDocumentProcess, map[string]interface{}{
				"tenant_id":             42,
				"knowledge_id":          "knowledge-1",
				"knowledge_base_id":     "kb-1",
				"processing_generation": "generation-1",
				"processing_owner":      "owner-1",
			}), errors.New("stale document task exhausted"))

			repo.mu.Lock()
			defer repo.mu.Unlock()
			if repo.documentCalls != 1 || repo.postProcessCalls != 0 {
				t.Fatalf("document/postprocess calls = %d/%d, want 1/0", repo.documentCalls, repo.postProcessCalls)
			}
			if repo.row.status != tt.status || repo.row.errorMessage != "current state" {
				t.Fatalf("row mutated to status=%q error=%q", repo.row.status, repo.row.errorMessage)
			}
			if repo.row.pendingSubtasks != 5 || repo.row.owner != tt.rowOwner || !repo.row.hasFanout {
				t.Fatalf("row cleanup state mutated: pending=%d owner=%q fanout=%v",
					repo.row.pendingSubtasks, repo.row.owner, repo.row.hasFanout)
			}
		})
	}
}

func TestDeadLetterPostProcessPendingIsNotEligible(t *testing.T) {
	repo := &deadLetterFenceFake{row: deadLetterFenceRow{
		tenantID:        42,
		knowledgeID:     "knowledge-1",
		knowledgeBaseID: "kb-1",
		generation:      "generation-1",
		status:          types.ParseStatusPending,
		pendingSubtasks: 3,
		hasFanout:       true,
		errorMessage:    "still waiting for core worker",
	}}
	callback := newDeadLetterKnowledgeStatusFailer(repo, repo, nil)
	callback(context.Background(), deadLetterTaskForTest(t, types.TypeKnowledgePostProcess, map[string]interface{}{
		"tenant_id":             42,
		"knowledge_id":          "knowledge-1",
		"knowledge_base_id":     "kb-1",
		"processing_generation": "generation-1",
	}), errors.New("stale postprocess task exhausted"))

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.documentCalls != 0 || repo.postProcessCalls != 1 {
		t.Fatalf("document/postprocess calls = %d/%d, want 0/1", repo.documentCalls, repo.postProcessCalls)
	}
	if repo.row.status != types.ParseStatusPending || repo.row.errorMessage != "still waiting for core worker" {
		t.Fatalf("pending row mutated to status=%q error=%q", repo.row.status, repo.row.errorMessage)
	}
	if repo.row.pendingSubtasks != 3 || !repo.row.hasFanout {
		t.Fatalf("pending row cleanup state mutated: pending=%d fanout=%v", repo.row.pendingSubtasks, repo.row.hasFanout)
	}
}

func TestDeadLetterPostProcessRequiresCommittedCore(t *testing.T) {
	repo := &deadLetterFenceFake{row: deadLetterFenceRow{
		tenantID:        42,
		knowledgeID:     "knowledge-1",
		knowledgeBaseID: "kb-1",
		generation:      "generation-1",
		status:          types.ParseStatusProcessing,
		processed:       false,
		pendingSubtasks: 3,
		hasFanout:       true,
		errorMessage:    "core still uncommitted",
	}}
	callback := newDeadLetterKnowledgeStatusFailer(repo, repo, nil)
	callback(context.Background(), deadLetterTaskForTest(t, types.TypeKnowledgePostProcess, map[string]interface{}{
		"tenant_id":             42,
		"knowledge_id":          "knowledge-1",
		"knowledge_base_id":     "kb-1",
		"processing_generation": "generation-1",
	}), errors.New("postprocess exhausted before core commit"))

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.row.status != types.ParseStatusProcessing || repo.row.errorMessage != "core still uncommitted" {
		t.Fatalf("uncommitted row mutated to status=%q error=%q", repo.row.status, repo.row.errorMessage)
	}
}

func TestDeadLetterKnowledgeStatusFailerIncompleteIdentityNeverMutates(t *testing.T) {
	complete := map[string]interface{}{
		"tenant_id":             42,
		"knowledge_id":          "knowledge-1",
		"knowledge_base_id":     "kb-1",
		"processing_generation": "generation-1",
		"processing_owner":      "owner-1",
	}
	tests := []struct {
		name     string
		taskType string
		payload  map[string]interface{}
	}{
		{name: "missing tenant", payload: withoutDeadLetterField(complete, "tenant_id")},
		{name: "missing knowledge", payload: withoutDeadLetterField(complete, "knowledge_id")},
		{name: "missing knowledge base", payload: withoutDeadLetterField(complete, "knowledge_base_id")},
		{name: "missing generation", payload: withoutDeadLetterField(complete, "processing_generation")},
		{name: "missing owner", payload: withoutDeadLetterField(complete, "processing_owner")},
		{name: "legacy manual generation is not accepted", taskType: types.TypeManualProcess, payload: map[string]interface{}{
			"tenant_id":         42,
			"knowledge_id":      "knowledge-1",
			"knowledge_base_id": "kb-1",
			"generation":        "generation-1",
			"processing_owner":  "owner-1",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &deadLetterFenceFake{row: deadLetterFenceRow{
				tenantID:        42,
				knowledgeID:     "knowledge-1",
				knowledgeBaseID: "kb-1",
				generation:      "generation-1",
				owner:           "owner-1",
				status:          types.ParseStatusProcessing,
			}}
			callback := newDeadLetterKnowledgeStatusFailer(repo, repo, nil)
			taskType := tt.taskType
			if taskType == "" {
				taskType = types.TypeDocumentProcess
			}
			callback(context.Background(), deadLetterTaskForTest(t, taskType, tt.payload), errors.New("boom"))

			repo.mu.Lock()
			defer repo.mu.Unlock()
			if repo.calls != 0 {
				t.Fatalf("CAS calls = %d, want 0 for incomplete identity", repo.calls)
			}
			if repo.row.status != types.ParseStatusProcessing {
				t.Fatalf("status = %q, want unchanged processing", repo.row.status)
			}
		})
	}
}

func withoutDeadLetterField(source map[string]interface{}, field string) map[string]interface{} {
	result := make(map[string]interface{}, len(source)-1)
	for key, value := range source {
		if key != field {
			result[key] = value
		}
	}
	return result
}
