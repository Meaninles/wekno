package documentsplit

import "fmt"

const (
	TypePartProcess = "document:split_part"
	TypeFinalize    = "document:split_finalize"
	QueuePart       = "document_part"
)

type PartPayload struct {
	TenantID             uint64 `json:"tenant_id"`
	KnowledgeBaseID      string `json:"knowledge_base_id"`
	KnowledgeID          string `json:"knowledge_id"`
	ProcessingGeneration string `json:"processing_generation"`
	PlanID               string `json:"plan_id"`
	PartID               string `json:"part_id"`
	PartIndex            int    `json:"part_index"`
	Attempt              int    `json:"attempt"`
	DeliveryEpoch        int64  `json:"delivery_epoch"`
}

type FinalizePayload struct {
	TenantID             uint64 `json:"tenant_id"`
	KnowledgeBaseID      string `json:"knowledge_base_id"`
	KnowledgeID          string `json:"knowledge_id"`
	ProcessingGeneration string `json:"processing_generation"`
	ProcessingOwner      string `json:"processing_owner"`
	PlanID               string `json:"plan_id"`
	Attempt              int    `json:"attempt"`
}

func PartTaskID(planID string, partIndex int, deliveryEpoch int64) string {
	if deliveryEpoch <= 0 {
		deliveryEpoch = 1
	}
	// A task ID is stable only for one database lease delivery. If a worker
	// dies after Asynq has retained the old task as "completed" but before the
	// database completion commits, recovery must be able to publish the next
	// fenced delivery instead of colliding with that retained ID.
	return fmt.Sprintf(
		"document-split:%s:part:%06d:delivery:%06d",
		planID, partIndex, deliveryEpoch,
	)
}

func FinalizeTaskID(planID string) string {
	return "document-split:" + planID + ":finalize"
}
