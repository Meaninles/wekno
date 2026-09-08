package agentruntime

import (
	"context"
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// The committed run is the delivery authority for streaming, history and model
// context. Staged uploads from an unfinished run must not appear as delivered.
func (s *Service) attachMessageArtifacts(ctx context.Context, messages []*types.Message) error {
	groups := map[string][]*types.Message{}
	for _, m := range messages {
		if m != nil && m.Role == "assistant" {
			groups[m.SessionID] = append(groups[m.SessionID], m)
		}
	}
	for session, group := range groups {
		ids := make([]string, 0, len(group))
		byID := map[string]*types.Message{}
		for _, m := range group {
			ids = append(ids, m.ID)
			byID[m.ID] = m
			m.Artifacts = nil
			m.ArtifactNotice = ""
		}
		var rows []RunRecord
		// Callers have already authorized the messages; scope by both their session
		// and message identities, never by a supplied artifact ID or answer URL.
		if err := s.db.WithContext(ctx).Select("message_id", "result").Where("session_id = ? AND message_id IN ? AND status IN ?", session, ids, []string{"completed", "incomplete"}).Order("created_at").Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			var result ChatResult
			if err := json.Unmarshal(row.Result, &result); err != nil {
				return err
			}
			if m := byID[row.MessageID]; m != nil {
				m.Artifacts = result.Artifacts
				m.ArtifactNotice = result.ArtifactNotice
			}
		}
	}
	return nil
}

func (s *Service) EnrichMessageArtifacts(ctx context.Context, messages []*types.Message) {
	if err := s.attachMessageArtifacts(ctx, messages); err != nil {
		logger.Warnf(ctx, "Load message artifact delivery: %v", err)
	}
}
