package wikidelete

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	quarantineMetadataKey = "_weknora_delete_quarantine"
	appliedOpsMetadataKey = "_weknora_applied_retract_ops"
	maxAppliedOpsPerPage  = 128
)

type quarantineState struct {
	OriginalStatus string   `json:"original_status"`
	Sources        []string `json:"sources"`
}

type quarantineClearContextKey struct{}

// WithQuarantineClear authorizes one Wiki write to complete quarantine for the
// exact source IDs supplied by the durable retract reducer. Carrying the IDs,
// rather than a boolean capability, prevents a trusted-but-stale write from
// blindly clearing a marker added for a different deletion operation.
func WithQuarantineClear(ctx context.Context, sourceIDs ...string) context.Context {
	return context.WithValue(ctx, quarantineClearContextKey{}, stableSources(sourceIDs))
}

// ClearSources returns the exact source IDs this write is authorized to remove
// from deletion quarantine. The returned slice is detached from the context so
// callers cannot mutate authority observed by another layer.
func ClearSources(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	sources, _ := ctx.Value(quarantineClearContextKey{}).([]string)
	return append([]string(nil), sources...)
}

// Quarantine hides a page before deletion cleanup starts while retaining its
// source_refs. The durable worker therefore still has the provenance needed to
// synthesize a safe shared-source body. Repeated calls union source IDs and
// retain the status that preceded the first quarantine.
func Quarantine(page *types.WikiPage, sourceIDs ...string) error {
	if page == nil {
		return errors.New("wiki delete quarantine: nil page")
	}
	metadata, state, err := decodeQuarantine(page.PageMetadata)
	if err != nil {
		return err
	}
	if state.OriginalStatus == "" {
		state.OriginalStatus = normalizedOriginalStatus(page.Status)
	}
	state.Sources = stableSources(state.Sources, sourceIDs)
	if len(state.Sources) == 0 {
		return errors.New("wiki delete quarantine: at least one source is required")
	}
	page.Status = types.WikiPageStatusArchived
	page.PageMetadata, err = encodeQuarantine(metadata, &state)
	return err
}

// Complete removes successfully-applied source IDs from the quarantine. The
// original visibility is restored only after every concurrent delete marker
// on the page has been applied.
func Complete(page *types.WikiPage, sourceIDs ...string) error {
	if page == nil {
		return errors.New("wiki delete quarantine: nil page")
	}
	metadata, state, err := decodeQuarantine(page.PageMetadata)
	if err != nil {
		return err
	}
	if len(state.Sources) == 0 {
		return nil
	}
	removed := make(map[string]struct{}, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		if sourceID = strings.TrimSpace(sourceID); sourceID != "" {
			removed[sourceID] = struct{}{}
		}
	}
	remaining := state.Sources[:0]
	for _, sourceID := range state.Sources {
		if _, drop := removed[sourceID]; !drop {
			remaining = append(remaining, sourceID)
		}
	}
	state.Sources = remaining
	if len(state.Sources) == 0 {
		delete(metadata, quarantineMetadataKey)
		page.Status = normalizedOriginalStatus(state.OriginalStatus)
		page.PageMetadata, err = encodeMetadata(metadata)
		return err
	}
	page.Status = types.WikiPageStatusArchived
	page.PageMetadata, err = encodeQuarantine(metadata, &state)
	return err
}

// CompleteAuthorized rebuilds incoming's quarantine marker from the current
// database row and then removes only the explicitly-authorized source IDs.
// Other incoming metadata is retained (for example applied retract operation
// IDs), but an absent or stale incoming quarantine marker is never trusted.
//
// Callers must still protect current with an optimistic-version check. This
// helper defines the merge semantics; it does not provide serialization.
func CompleteAuthorized(current, incoming *types.WikiPage, sourceIDs ...string) error {
	if current == nil || incoming == nil {
		return errors.New("wiki delete quarantine: current and incoming pages are required")
	}
	currentMetadata, currentState, err := decodeQuarantine(current.PageMetadata)
	if err != nil {
		return err
	}
	incomingMetadata, _, err := decodeQuarantine(incoming.PageMetadata)
	if err != nil {
		return err
	}

	// The marker is sourced exclusively from the locked/current row. The
	// caller may contribute other metadata, but never quarantine authority.
	delete(incomingMetadata, quarantineMetadataKey)
	if len(currentState.Sources) == 0 {
		incoming.PageMetadata, err = encodeMetadata(incomingMetadata)
		if err != nil {
			return err
		}
		incoming.Status = current.Status
	} else {
		encodedState, err := json.Marshal(&currentState)
		if err != nil {
			return err
		}
		incomingMetadata[quarantineMetadataKey] = encodedState
		incoming.PageMetadata, err = encodeMetadata(incomingMetadata)
		if err != nil {
			return err
		}
		incoming.Status = types.WikiPageStatusArchived
	}

	// Concurrent trusted completions do not bump the user revision. Preserve
	// durable applied-operation markers already committed by an earlier
	// completion before writing this caller's metadata snapshot.
	if err := mergeAppliedOpsFromCurrent(currentMetadata, incoming); err != nil {
		return err
	}
	if len(currentState.Sources) == 0 {
		return nil
	}
	return Complete(incoming, sourceIDs...)
}

// Preserve copies the authoritative quarantine marker/status from the current
// database row onto an untrusted incoming write. This closes the race where an
// ingest worker loaded a published page before deletion quarantined it and
// later attempted to write that stale status back.
func Preserve(current, incoming *types.WikiPage) error {
	if current == nil || incoming == nil {
		return nil
	}
	currentMetadata, state, err := decodeQuarantine(current.PageMetadata)
	if err != nil {
		return err
	}
	incomingMetadata, incomingState, err := decodeQuarantine(incoming.PageMetadata)
	if err != nil {
		return err
	}
	if len(state.Sources) == 0 {
		return mergeAppliedOpsFromCurrent(currentMetadata, incoming)
	}
	// An incoming write can itself be a new deletion quarantine. Union it
	// with the locked/current marker instead of replacing either side. This is
	// what makes K1+K2 concurrent deletion of one shared page safe.
	state.Sources = stableSources(state.Sources, incomingState.Sources)
	incoming.PageMetadata, err = encodeQuarantine(incomingMetadata, &state)
	if err != nil {
		return err
	}
	incoming.Status = types.WikiPageStatusArchived
	return mergeAppliedOpsFromCurrent(currentMetadata, incoming)
}

func mergeAppliedOpsFromCurrent(currentMetadata map[string]json.RawMessage, incoming *types.WikiPage) error {
	if incoming == nil {
		return nil
	}
	var currentApplied []int64
	if value := currentMetadata[appliedOpsMetadataKey]; len(value) > 0 {
		if err := json.Unmarshal(value, &currentApplied); err != nil {
			return errors.New("wiki delete quarantine: invalid applied-op marker: " + err.Error())
		}
	}
	incomingMetadata, _, err := decodeQuarantine(incoming.PageMetadata)
	if err != nil {
		return err
	}
	var incomingApplied []int64
	if value := incomingMetadata[appliedOpsMetadataKey]; len(value) > 0 {
		if err := json.Unmarshal(value, &incomingApplied); err != nil {
			return errors.New("wiki delete quarantine: invalid applied-op marker: " + err.Error())
		}
	}

	seen := make(map[int64]struct{}, len(currentApplied)+len(incomingApplied))
	merged := make([]int64, 0, len(currentApplied)+len(incomingApplied))
	for _, id := range append(currentApplied, incomingApplied...) {
		if id <= 0 {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		merged = append(merged, id)
	}
	if len(merged) == 0 {
		return nil
	}
	if len(merged) > maxAppliedOpsPerPage {
		merged = merged[len(merged)-maxAppliedOpsPerPage:]
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	incomingMetadata[appliedOpsMetadataKey] = encoded
	incoming.PageMetadata, err = encodeMetadata(incomingMetadata)
	return err
}

// PendingSources returns the deletion source IDs currently quarantining page.
func PendingSources(page *types.WikiPage) ([]string, error) {
	if page == nil {
		return nil, nil
	}
	_, state, err := decodeQuarantine(page.PageMetadata)
	return append([]string(nil), state.Sources...), err
}

// IsApplied lets a durable retract retry distinguish "the source ref is gone
// because this exact operation already committed" from legacy/partial cleanup
// that removed metadata before the shared page body was synthesized.
func IsApplied(page *types.WikiPage, sourceOpID int64) (bool, error) {
	if page == nil || sourceOpID <= 0 {
		return false, nil
	}
	metadata, _, err := decodeQuarantine(page.PageMetadata)
	if err != nil {
		return false, err
	}
	var applied []int64
	if value := metadata[appliedOpsMetadataKey]; len(value) > 0 {
		if err := json.Unmarshal(value, &applied); err != nil {
			return false, errors.New("wiki delete quarantine: invalid applied-op marker: " + err.Error())
		}
	}
	for _, id := range applied {
		if id == sourceOpID {
			return true, nil
		}
	}
	return false, nil
}

// MarkApplied records retract operation IDs in bounded page metadata. The
// marker is written atomically with the cleaned body/source refs by UpdatePage,
// so failures in later logging/index/queue settlement never repeat the LLM edit.
func MarkApplied(page *types.WikiPage, sourceOpIDs ...int64) error {
	if page == nil {
		return errors.New("wiki delete quarantine: nil page")
	}
	metadata, _, err := decodeQuarantine(page.PageMetadata)
	if err != nil {
		return err
	}
	var applied []int64
	if value := metadata[appliedOpsMetadataKey]; len(value) > 0 {
		if err := json.Unmarshal(value, &applied); err != nil {
			return errors.New("wiki delete quarantine: invalid applied-op marker: " + err.Error())
		}
	}
	seen := make(map[int64]struct{}, len(applied)+len(sourceOpIDs))
	result := make([]int64, 0, len(applied)+len(sourceOpIDs))
	for _, id := range append(applied, sourceOpIDs...) {
		if id <= 0 {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) > maxAppliedOpsPerPage {
		result = result[len(result)-maxAppliedOpsPerPage:]
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	metadata[appliedOpsMetadataKey] = encoded
	page.PageMetadata, err = encodeMetadata(metadata)
	return err
}

func decodeQuarantine(raw types.JSON) (map[string]json.RawMessage, quarantineState, error) {
	metadata := make(map[string]json.RawMessage)
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, quarantineState{}, errors.New("wiki delete quarantine: invalid page metadata: " + err.Error())
		}
	}
	var state quarantineState
	if value := metadata[quarantineMetadataKey]; len(value) > 0 {
		if err := json.Unmarshal(value, &state); err != nil {
			return nil, quarantineState{}, errors.New("wiki delete quarantine: invalid marker: " + err.Error())
		}
		state.Sources = stableSources(state.Sources, nil)
	}
	return metadata, state, nil
}

func encodeQuarantine(metadata map[string]json.RawMessage, state *quarantineState) (types.JSON, error) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	metadata[quarantineMetadataKey] = encoded
	return encodeMetadata(metadata)
}

func encodeMetadata(metadata map[string]json.RawMessage) (types.JSON, error) {
	if len(metadata) == 0 {
		return types.JSON([]byte("{}")), nil
	}
	encoded, err := json.Marshal(metadata)
	return types.JSON(encoded), err
}

func stableSources(groups ...[]string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, group := range groups {
		for _, sourceID := range group {
			sourceID = strings.TrimSpace(sourceID)
			if sourceID == "" {
				continue
			}
			if _, exists := seen[sourceID]; exists {
				continue
			}
			seen[sourceID] = struct{}{}
			result = append(result, sourceID)
		}
	}
	sort.Strings(result)
	return result
}

func normalizedOriginalStatus(status string) string {
	switch status {
	case types.WikiPageStatusDraft, types.WikiPageStatusPublished, types.WikiPageStatusArchived:
		return status
	default:
		return types.WikiPageStatusPublished
	}
}
