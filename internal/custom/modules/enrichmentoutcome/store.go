package enrichmentoutcome

import "context"

// Aggregate is the generation-scoped terminal result summary shared by the
// core document fan-out (for example multimodal images) and the later
// enrichment fan-out (summary, questions and graph extraction).
type Aggregate struct {
	Total     int64 `gorm:"column:total"`
	Failed    int64 `gorm:"column:failed"`
	Degraded  int64 `gorm:"column:degraded"`
	Completed int64 `gorm:"column:completed"`
}

// Status returns the externally visible aggregate status. An empty generation
// has no enrichment outcome, while any failed/degraded item prevents a false
// all-completed result.
func (a Aggregate) Status() string {
	switch {
	case a.Total == 0:
		return ""
	case a.Failed == a.Total:
		return StatusFailed
	case a.Failed > 0 || a.Degraded > 0:
		return StatusDegraded
	default:
		return StatusCompleted
	}
}

// GenerationStore is deliberately a narrow optional repository extension.
// Keeping it here avoids widening the upstream KnowledgeRepository interface
// while allowing core fan-out workers and terminal repair to persist outcomes
// before the document transitions from processing to finalizing.
type GenerationStore interface {
	RecordGenerationOutcome(
		context.Context,
		uint64,
		string,
		string,
		string,
		string,
		string,
		string,
	) (bool, error)
	GetGenerationOutcomeAggregate(
		context.Context,
		uint64,
		string,
		string,
		string,
	) (Aggregate, error)
}
