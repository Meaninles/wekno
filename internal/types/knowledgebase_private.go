package types

import "context"

// AllowsPrivateAccess is independent of tenant roles and sharing grants. A
// tenant administrator or shared agent cannot inherit another user's uploads.
func (kb *KnowledgeBase) AllowsPrivateAccess(ctx context.Context) bool {
	if kb == nil {
		return false
	}
	if kb.ChatSessionID == "" {
		return true
	}
	tenant, ok := TenantIDFromContext(ctx)
	return ok && tenant == kb.TenantID && kb.ChatOwnerID != "" &&
		kb.ChatOwnerID == SessionOwnerIDFromContext(ctx)
}
