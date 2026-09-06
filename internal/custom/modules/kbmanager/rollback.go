package kbmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// rollback owns only the unpublished resource created by this accepted
// operation. Cleanup remains durable when a chat ends or caller access is
// revoked; it never uses that authority to delete a published user document.
func (s *Service) rollback(ctx context.Context, op *Operation) {
	var created types.Knowledge
	err := s.db.WithContext(ctx).Unscoped().Where("id = ? AND tenant_id = ? AND knowledge_base_id = ?", op.NewKnowledgeID, op.SourceTenantID, op.KnowledgeBaseID).First(&created).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		op.ResultMessage = "未发布版本清理待重试: " + err.Error()
		return
	}
	if err == nil {
		var metadata map[string]any
		if json.Unmarshal(created.Metadata, &metadata) != nil || metadata["management_operation_id"] != op.ID || created.PublicationState != "staged" || created.ProcessingGeneration != op.NewGeneration {
			op.ResultMessage = "替换失败；未发布版本身份已由其他操作接管，旧文档保留"
			op.State = OperationStateFailed
			now := time.Now()
			op.CompletedAt = &now
			return
		}
		sourceCtx, err := s.cleanupContext(ctx, op)
		if err != nil {
			op.ResultMessage = "未发布版本清理待重试: " + err.Error()
			return
		}
		if err := s.knowledgeService.DeleteKnowledge(sourceCtx, created.ID); err != nil {
			op.ResultMessage = "未发布版本清理待重试: " + err.Error()
			return
		}
	}
	now := time.Now()
	op.State = OperationStateFailed
	op.CompletedAt = &now
	op.ResultMessage = "替换失败；旧文档保留，未发布的新版本已清理"
}

func (s *Service) cleanupContext(ctx context.Context, op *Operation) (context.Context, error) {
	tenant, err := s.tenantService.GetTenantByID(ctx, op.SourceTenantID)
	if err != nil || tenant == nil {
		return nil, fmt.Errorf("source tenant unavailable for committed cleanup: %v", err)
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, op.SourceTenantID)
	return context.WithValue(ctx, types.TenantInfoContextKey, tenant), nil
}
