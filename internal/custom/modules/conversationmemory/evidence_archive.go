package conversationmemory

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// EvidenceArchive is a read projection of durable runs, not another evidence
// store. Message citations are a display subset and must never gate reuse.
type EvidenceArchive struct {
	MessageID  string
	References []*types.SearchResult `gorm:"serializer:json"`
}

func LoadEvidenceArchive(ctx context.Context, db *gorm.DB, sessionID string, messageIDs []string) (map[string][]*types.SearchResult, error) {
	out := map[string][]*types.SearchResult{}
	tenant, ok := types.TenantIDFromContext(ctx)
	if !ok || tenant == 0 || len(messageIDs) == 0 {
		return out, nil
	}
	var rows []EvidenceArchive
	err := db.WithContext(ctx).Table("custom_agent_runs").Select("message_id", "references").
		Where("session_id = ? AND tenant_id = ? AND message_id IN ? AND status = ?", sessionID, tenant, messageIDs, "completed").Find(&rows).Error
	for _, row := range rows {
		out[row.MessageID] = row.References
	}
	return out, err
}

// WithArchivedEvidence returns copies for context construction only. It never
// changes the final citations persisted on a message.
func WithArchivedEvidence(ctx context.Context, db *gorm.DB, sessionID string, messages []*types.Message, targets types.SearchTargets) ([]*types.Message, error) {
	ids := []string{}
	for _, m := range messages {
		if m == nil {
			continue
		}
		if m.Role == "assistant" {
			ids = append(ids, m.ID)
		}
	}
	archive, err := LoadEvidenceArchive(ctx, db, sessionID, ids)
	if err != nil {
		return nil, err
	}
	all := []*types.SearchResult{}
	for _, refs := range archive {
		all = append(all, refs...)
	}
	valid, _, err := sourcerefs.ReuseEvidence(ctx, db, targets, all)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, ref := range valid {
		allowed[evidenceKey(ref)] = true
	}
	out := make([]*types.Message, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}
		cp := *m
		// Catalogs are navigation, not permission grants. Hide titles outside the
		// active selection; exact content/version authorization occurs on each read.
		cp.KnowledgeReferences = nil
		refs := m.KnowledgeReferences
		if saved, exists := archive[m.ID]; exists {
			refs = saved
		}
		for _, ref := range refs {
			if ref != nil && allowed[evidenceKey(ref)] {
				cp.KnowledgeReferences = append(cp.KnowledgeReferences, ref)
			}
		}
		out = append(out, &cp)
	}
	return out, nil
}

func evidenceInScope(ref *types.SearchResult, targets types.SearchTargets) bool {
	if ref == nil {
		return false
	}
	for _, target := range targets {
		if target == nil || target.TenantID == 0 || target.KnowledgeBaseID != ref.KnowledgeBaseID {
			continue
		}
		if target.Type != types.SearchTargetTypeKnowledge {
			return true
		}
		for _, id := range target.KnowledgeIDs {
			if id == ref.KnowledgeID {
				return true
			}
		}
	}
	return false
}

func evidenceKey(ref *types.SearchResult) string {
	return ref.KnowledgeBaseID + ":" + ref.ID + ":" + ref.Metadata[sourcerefs.MetadataEvidenceHash]
}

// SelectEvidence is a bounded lexical selection over already retrieved source
// snapshots. It performs no model call and is not a replacement for retrieval.
// Exact source_id/page reads remain available when this cache misses a topic.
func SelectEvidence(refs []*types.SearchResult, query string, budget int) []*types.SearchResult {
	terms := evidenceTerms(query)
	type candidate struct {
		ref   *types.SearchResult
		score int
	}
	candidates := []candidate{}
	seen := map[string]bool{}
	for _, ref := range refs {
		if ref == nil || !sourcerefs.IsSupportedCitationReference(ref) {
			continue
		}
		key := evidenceKey(ref)
		if seen[key] {
			continue
		}
		seen[key] = true
		body, title := strings.ToLower(ref.EvidenceContent), strings.ToLower(ref.KnowledgeTitle)
		score := 0
		for term := range terms {
			if strings.Contains(body, term) {
				score++
			}
			if strings.Contains(title, term) {
				score += 3
			}
		}
		if score > 0 {
			candidates = append(candidates, candidate{ref, score})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	out := []*types.SearchResult{}
	for _, c := range candidates {
		cost := EstimateTokens(c.ref.EvidenceContent) + EstimateTokens(c.ref.KnowledgeTitle) + 100
		if cost > budget {
			continue
		}
		out = append(out, c.ref)
		budget -= cost
		if len(out) == 8 {
			break
		}
	}
	return out
}

func evidenceTerms(text string) map[string]bool {
	terms := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		runes := []rune(word)
		if len(runes) < 2 {
			continue
		}
		terms[word] = true
		for i := 0; i+1 < len(runes); i++ {
			if unicode.Is(unicode.Han, runes[i]) && unicode.Is(unicode.Han, runes[i+1]) {
				terms[string(runes[i:i+2])] = true
			}
		}
	}
	return terms
}
