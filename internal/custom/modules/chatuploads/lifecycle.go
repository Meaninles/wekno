package chatuploads

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// Deletion reuses the native durable KB outbox. The periodic orphan scan
// recovers process exits between deleting a session and notifying its hooks.
func (s *Service) DeleteSessionUploads(ctx context.Context, tenantID uint64, owner string, sessionIDs []string) error {
	if len(sessionIDs) == 0 || tenantID == 0 {
		return nil
	}
	var rows []types.KnowledgeBase
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND chat_owner_id = ? AND chat_session_id IN ?", tenantID, owner, sessionIDs).Find(&rows).Error; err != nil {
		return err
	}
	if err := s.deleteOrphans(ctx, rows); err != nil {
		return err
	}
	var originals []*OriginalUpload
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND owner_id = ? AND session_id IN ? AND deleted_at IS NULL", tenantID, owner, sessionIDs).Find(&originals).Error; err != nil {
		return err
	}
	var failures []error
	for _, row := range originals {
		if err := s.withAcceptance(ctx, &types.Session{ID: row.SessionID, TenantID: row.TenantID}, func() error {
			var alive int64
			if err := s.db.WithContext(ctx).Model(&types.Session{}).Where("id = ? AND tenant_id = ?", row.SessionID, row.TenantID).Count(&alive).Error; err != nil {
				return err
			}
			if alive > 0 {
				return nil
			}
			return s.deleteOriginalRow(ctx, row)
		}); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) sweep(ctx context.Context) error {
	var rows []types.KnowledgeBase
	err := s.db.WithContext(ctx).Raw(`SELECT kb.* FROM knowledge_bases kb
   WHERE kb.deleted_at IS NULL AND kb.chat_session_id <> '' AND NOT EXISTS
   (SELECT 1 FROM sessions s WHERE s.id = kb.chat_session_id AND s.tenant_id = kb.tenant_id AND s.deleted_at IS NULL)
   ORDER BY kb.created_at, kb.id LIMIT 100`).Scan(&rows).Error
	if err != nil {
		return err
	}
	if err := s.deleteOrphans(ctx, rows); err != nil {
		return err
	}
	var originals []*OriginalUpload
	if err := s.db.WithContext(ctx).Raw(`SELECT o.* FROM custom_chat_original_uploads o
		WHERE o.deleted_at IS NULL AND NOT EXISTS
		(SELECT 1 FROM sessions ss WHERE ss.id = o.session_id AND ss.tenant_id = o.tenant_id AND ss.deleted_at IS NULL)
		ORDER BY o.created_at, o.id LIMIT 100`).Scan(&originals).Error; err != nil {
		return err
	}
	var failures []error
	for _, row := range originals {
		if err := s.deleteOriginalRow(ctx, row); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) deleteOrphans(ctx context.Context, rows []types.KnowledgeBase) error {
	var failures []error
	for _, kb := range rows {
		err := s.withAcceptance(ctx, &types.Session{ID: kb.ChatSessionID, TenantID: kb.TenantID}, func() error {
			var alive int64
			if err := s.db.WithContext(ctx).Model(&types.Session{}).Where("id = ? AND tenant_id = ?", kb.ChatSessionID, kb.TenantID).Count(&alive).Error; err != nil {
				return err
			}
			if alive > 0 {
				return nil
			}
			var tenant types.Tenant
			if err := s.db.WithContext(ctx).Where("id = ?", kb.TenantID).Take(&tenant).Error; err != nil {
				return err
			}
			scoped := context.WithValue(ctx, types.TenantIDContextKey, kb.TenantID)
			scoped = context.WithValue(scoped, types.TenantInfoContextKey, &tenant)
			return s.kbs.DeleteKnowledgeBase(scoped, kb.ID)
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) Start(ctx context.Context) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if s.maintenanceCancel != nil {
		return
	}
	active, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.maintenanceCancel, s.maintenanceDone = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if err := s.sweep(active); err != nil && active.Err() == nil {
				logger.Warnf(active, "chat source cleanup will retry: %v", err)
			}
			select {
			case <-active.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (s *Service) Stop() {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if s.maintenanceCancel == nil {
		return
	}
	s.maintenanceCancel()
	<-s.maintenanceDone
	s.maintenanceCancel, s.maintenanceDone = nil, nil
}
