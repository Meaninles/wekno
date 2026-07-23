// Package corefanout recovers the durable downstream fan-out recorded when a
// document's core chunks and indexes have already committed.
package corefanout

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/redis/go-redis/v9"
)

// HasCommittedPlan identifies the post-core/pre-fanout durability boundary.
// It deliberately does not parse the plan: housekeeping must preserve a
// malformed plan for diagnosis and repair instead of silently completing or
// failing a row whose searchable artifacts have already committed.
func HasCommittedPlan(knowledge *types.Knowledge) bool {
	return knowledge != nil &&
		knowledge.ParseStatus == types.ParseStatusProcessing &&
		strings.TrimSpace(knowledge.ProcessingOwner) == "" &&
		knowledge.ProcessedAt != nil &&
		len(strings.TrimSpace(string(knowledge.ProcessingFanout))) > 0
}

// ParseExact validates both the plan schema and every persisted row identity.
// Partial or mismatched state fails closed and is never cleared by recovery.
func ParseExact(knowledge *types.Knowledge) (processownership.FanoutPlan, error) {
	var empty processownership.FanoutPlan
	if !HasCommittedPlan(knowledge) {
		return empty, errors.New("knowledge is not at the committed core fanout boundary")
	}
	if knowledge.TenantID == 0 || strings.TrimSpace(knowledge.ID) == "" ||
		strings.TrimSpace(knowledge.KnowledgeBaseID) == "" ||
		strings.TrimSpace(knowledge.ProcessingGeneration) == "" {
		return empty, errors.New("committed core fanout row has incomplete identity")
	}
	plan, err := processownership.ParseFanoutPlan(knowledge.ProcessingFanout)
	if err != nil {
		return empty, fmt.Errorf("parse persisted fanout plan: %w", err)
	}
	if plan.TenantID != knowledge.TenantID || plan.KnowledgeID != knowledge.ID ||
		plan.KnowledgeBaseID != knowledge.KnowledgeBaseID ||
		plan.ProcessingGeneration != knowledge.ProcessingGeneration {
		return empty, errors.New("persisted fanout plan identity mismatch")
	}
	return plan, nil
}

// Replay dispatches the exact persisted plan. Stable task IDs and the durable
// completion ledger make retries and concurrent replicas idempotent.
func Replay(
	ctx context.Context,
	enqueuer interfaces.TaskEnqueuer,
	redisClient *redis.Client,
	completionStore processownership.DurableFanoutCompletionStore,
	knowledge *types.Knowledge,
) error {
	if completionStore == nil {
		return errors.New("durable fanout completion store is unavailable")
	}
	plan, err := ParseExact(knowledge)
	if err != nil {
		return err
	}
	return processownership.DispatchFanout(ctx, enqueuer, redisClient, plan, completionStore)
}
