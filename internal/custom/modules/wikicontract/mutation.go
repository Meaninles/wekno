package wikicontract

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiingestguard"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

var ErrPageConflict = errors.New("wiki page version or identity conflict")

// MutateIdentity changes an address (or deletes it) in one KB-fenced
// transaction. Page identity and provenance survive a rename. Links and issues
// cannot commit against a half-renamed page. Nil newSlug means delete.
func MutateIdentity(ctx context.Context, db *gorm.DB, page *types.WikiPage, newSlug *string) error {
	if page == nil || page.ID == "" || page.Version <= 0 {
		return ErrPageConflict
	}
	if newSlug != nil && (strings.TrimSpace(*newSlug) == "" || *newSlug == page.Slug) {
		return errors.New("new_slug must be a different nonempty slug")
	}
	return kbwritefence.WithActive(ctx, db, page.TenantID, page.KnowledgeBaseID, func(tx *gorm.DB) error {
		if err := wikiingestguard.ValidateScope(ctx, tx, page.TenantID, page.KnowledgeBaseID); err != nil {
			return err
		}
		query := tx.Unscoped().Model(&types.WikiPage{}).Where("id = ? AND tenant_id = ? AND knowledge_base_id = ? AND slug = ? AND version = ? AND deleted_at IS NULL", page.ID, page.TenantID, page.KnowledgeBaseID, page.Slug, page.Version)
		var result *gorm.DB
		if newSlug == nil {
			result = query.Delete(&types.WikiPage{})
		} else {
			result = query.Updates(map[string]any{"slug": *newSlug, "version": page.Version + 1, "updated_at": time.Now()})
		}
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrPageConflict
		}
		// Page data is bounded per batch; the KB write fence serializes all page
		// writers, so the expected-version updates below cannot lose an edit.
		var pages []types.WikiPage
		links := regexp.MustCompile(`\[\[` + regexp.QuoteMeta(page.Slug) + `(?:\|([^\]]+))?\]\]`)
		err := tx.Where("tenant_id = ? AND knowledge_base_id = ?", page.TenantID, page.KnowledgeBaseID).
			FindInBatches(&pages, 100, func(batch *gorm.DB, _ int) error {
				for _, p := range pages {
					body := links.ReplaceAllStringFunc(p.Content, func(link string) string {
						m := links.FindStringSubmatch(link)
						if newSlug != nil {
							return strings.Replace(link, "[["+page.Slug, "[["+*newSlug, 1)
						}
						if m[1] != "" {
							return m[1]
						}
						return page.Title
					})
					in, ci := replaceLink(p.InLinks, page.Slug, newSlug)
					out, co := replaceLink(p.OutLinks, page.Slug, newSlug)
					parent := p.ParentSlug
					if parent == page.Slug {
						parent = ""
						if newSlug != nil {
							parent = *newSlug
						}
					}
					if body == p.Content && !ci && !co && parent == p.ParentSlug {
						continue
					}
					values := map[string]any{"content": body, "in_links": in, "out_links": out, "parent_slug": parent, "updated_at": time.Now()}
					if body != p.Content {
						values["version"] = p.Version + 1
					}
					r := tx.Model(&types.WikiPage{}).Where("id = ? AND version = ?", p.ID, p.Version).Updates(values)
					if r.Error != nil {
						return r.Error
					}
					if r.RowsAffected != 1 {
						return ErrPageConflict
					}
				}
				return nil
			}).Error
		if err != nil {
			return err
		}
		issues := tx.Where("tenant_id = ? AND knowledge_base_id = ? AND slug = ?", page.TenantID, page.KnowledgeBaseID, page.Slug)
		if newSlug == nil {
			err = issues.Delete(&types.WikiPageIssue{}).Error
		} else {
			err = issues.Model(&types.WikiPageIssue{}).Update("slug", *newSlug).Error
		}
		if err != nil {
			return err
		}
		return wikiingestguard.RecordPageApplication(ctx, tx, page.Slug)
	})
}

func replaceLink(values types.StringArray, old string, next *string) (types.StringArray, bool) {
	out := make(types.StringArray, 0, len(values))
	changed := false
	for _, value := range values {
		if value == old {
			changed = true
			if next != nil {
				out = append(out, *next)
			}
		} else {
			out = append(out, value)
		}
	}
	return out, changed
}
