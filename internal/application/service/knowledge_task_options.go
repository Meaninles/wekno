package service

import (
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/custom/modules/fileguard"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

func assignNewDocumentProcessingIdentity(knowledge *types.Knowledge) {
	if knowledge == nil || knowledge.ID == "" {
		return
	}
	knowledge.ProcessingGeneration = uuid.NewString()
	knowledge.ProcessingOwner = processownership.DocumentOwner(knowledge.ID, knowledge.ProcessingGeneration)
	knowledge.ProcessingWorkflowID = ""
	knowledge.ProcessingFanout = nil
	knowledge.PendingSubtasksCount = 0
	knowledge.EnrichmentStatus = types.EnrichmentStatusNone
	knowledge.WikiStatus = types.WikiStatusNone
	knowledge.WikiErrorMessage = ""
}

func documentProcessOwnershipPayload(knowledge *types.Knowledge, payload *types.DocumentProcessPayload) {
	if knowledge == nil || payload == nil {
		return
	}
	payload.ProcessingGeneration = knowledge.ProcessingGeneration
	payload.ProcessingOwner = knowledge.ProcessingOwner
}

func documentProcessStableTaskID(knowledge *types.Knowledge, queue string) asynq.Option {
	return asynq.TaskID(processownership.DocumentTaskID(
		knowledge.ID,
		knowledge.ProcessingGeneration,
		queue,
	))
}

func documentProcessTaskOptions(cfg *config.Config, extra ...asynq.Option) []asynq.Option {
	return documentProcessTaskOptionsForQueue(cfg, types.QueueDocument, extra...)
}

func documentProcessTaskOptionsForQueue(cfg *config.Config, queue string, extra ...asynq.Option) []asynq.Option {
	// Root parsing is always scheduled as one complete document workflow.
	// Heavy-file classification remains useful for validation/observability,
	// but may no longer split one document lifecycle onto a task-type queue.
	queue = types.QueueDocument
	opts := []asynq.Option{
		asynq.Queue(queue),
		asynq.Timeout(config.DocumentProcessTimeout(cfg)),
	}
	opts = append(opts, extra...)
	return opts
}

func documentProcessQueueForReport(report fileguard.Report) string {
	return types.QueueDocument
}
