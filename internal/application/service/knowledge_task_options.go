package service

import (
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/custom/modules/fileguard"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
)

func documentProcessTaskOptions(cfg *config.Config, extra ...asynq.Option) []asynq.Option {
	return documentProcessTaskOptionsForQueue(cfg, types.QueueDefault, extra...)
}

func documentProcessTaskOptionsForQueue(cfg *config.Config, queue string, extra ...asynq.Option) []asynq.Option {
	if queue == "" {
		queue = types.QueueDefault
	}
	opts := []asynq.Option{
		asynq.Queue(queue),
		asynq.Timeout(config.DocumentProcessTimeout(cfg)),
	}
	opts = append(opts, extra...)
	return opts
}

func documentProcessQueueForReport(report fileguard.Report) string {
	if report.IsHeavy() {
		return types.QueueDocumentHeavy
	}
	return types.QueueDefault
}
