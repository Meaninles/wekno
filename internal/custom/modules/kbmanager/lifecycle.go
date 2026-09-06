package kbmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// reserve makes retries of a tool call refer to the same durable operation.
func (s *Service) reserve(ctx context.Context, op *Operation, data []byte) (*Operation, bool, error) {
	identity := fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%v", op.CallerTenantID, op.UserID, op.AgentID, op.RunID, op.Type, op.KnowledgeBaseID, op.OldKnowledgeID, op.OldFileHash, op.SourceSHA256, op.FileName, op.InputMetadata, op.InputTagIDs)
	hash := sha256.Sum256([]byte(identity))
	op.IdempotencyKey = hex.EncodeToString(hash[:])
	until := time.Now().Add(5 * time.Minute)
	op.LeaseUntil = &until // foreground acceptance owns preparing until checkpoint
	var fresh bool
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "idempotency_key"}}, DoNothing: true}).Create(op)
		if result.Error != nil {
			return result.Error
		}
		fresh = result.RowsAffected == 1
		if fresh && op.Type != OperationTypeDelete {
			return tx.Create(&OperationInput{OperationID: op.ID, Data: data}).Error
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if fresh {
		return op, true, nil
	}
	var existing Operation
	err = s.db.WithContext(ctx).Where("idempotency_key = ?", op.IdempotencyKey).First(&existing).Error
	return &existing, false, err
}

// One bounded coordinator reconciles persisted document lifecycle states.
// There is no per-operation polling goroutine. A database lease, not a
// process-local map, owns each transition across all API replicas.
func (s *Service) resumePending(ctx context.Context) {
	var operations []Operation
	err := s.db.WithContext(ctx).Where("state IN ? AND (lease_until IS NULL OR lease_until < ?)",
		[]string{OperationStatePreparing, OperationStateParsing, OperationStateCleanup, OperationStateRollback}, time.Now()).Order("updated_at, id").Limit(32).Find(&operations).Error
	if err != nil {
		logger.Warnf(ctx, "[kbmanager] reconcile: %v", err)
		return
	}
	for _, snapshot := range operations {
		if ctx.Err() != nil {
			return
		}
		owner := uuid.NewString()
		until := time.Now().Add(90 * time.Second)
		claimed := s.db.WithContext(ctx).Model(&Operation{}).Where("id = ? AND state = ? AND (lease_until IS NULL OR lease_until < ?)", snapshot.ID, snapshot.State, time.Now()).Updates(map[string]any{"lease_owner": owner, "lease_until": until})
		if claimed.Error != nil || claimed.RowsAffected != 1 {
			continue
		}
		workCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		op := snapshot
		op.LeaseOwner = owner
		op.LeaseUntil = &until
		s.advance(workCtx, &op)
		cancel()
		op.UpdatedAt = time.Now()
		op.LeaseOwner = ""
		op.LeaseUntil = nil
		// A cancelled/expired worker cannot overwrite its successor.
		checkpoint, release := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err := s.db.WithContext(checkpoint).Transaction(func(tx *gorm.DB) error {
			result := tx.Model(&Operation{}).Where("id = ? AND lease_owner = ?", op.ID, owner).Select("*").Updates(&op)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 1 && op.State != OperationStatePreparing {
				return tx.Where("operation_id = ?", op.ID).Delete(&OperationInput{}).Error
			}
			return nil
		})
		release()
		if err != nil {
			logger.Warnf(ctx, "[kbmanager] checkpoint: %v", err)
		}
	}
}

func (s *Service) kick(_ string) {
	s.Start()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) advance(ctx context.Context, op *Operation) {
	fail := func(err error) {
		now := time.Now()
		op.State = OperationStateFailed
		op.ErrorMessage = err.Error()
		op.CompletedAt = &now
		if op.Type == OperationTypeReplace && op.NewKnowledgeID != "" && !op.Searchable {
			op.State = OperationStateRollback
			op.CompletedAt = nil
			op.ResultMessage = "替换失败，旧文档已保留；正在清理未发布的新版本"
		}
	}
	complete := func(message string) {
		now := time.Now()
		op.State = OperationStateCompleted
		op.ResultMessage = message
		op.ErrorMessage = ""
		op.CompletedAt = &now
	}
	caller := context.WithValue(ctx, types.TenantIDContextKey, op.CallerTenantID)
	caller = context.WithValue(caller, types.UserIDContextKey, op.UserID)
	caller = context.WithValue(caller, types.TenantRoleContextKey, types.TenantRole(op.CallerRole))
	if op.State == OperationStateRollback {
		s.rollback(ctx, op)
		return
	}
	if op.Type == OperationTypeReplace && op.State == OperationStateCleanup && op.Searchable {
		sourceCtx, err := s.cleanupContext(ctx, op)
		if err != nil {
			op.ErrorMessage = err.Error()
			return
		}
		s.cleanup(sourceCtx, op, complete)
		return
	}
	_, sourceCtx, err := s.authorizeMutation(caller, op.KnowledgeBaseID)
	if err != nil {
		fail(err)
		return
	}
	if op.Type == OperationTypeDelete || op.State == OperationStateCleanup {
		s.cleanup(sourceCtx, op, complete)
		return
	}
	if op.State == OperationStatePreparing {
		// Ingestion stores this operation ID as native knowledge metadata. This
		// closes a crash between creating the knowledge and saving its ID.
		var created types.Knowledge
		err = s.db.WithContext(ctx).Where("tenant_id = ? AND knowledge_base_id = ? AND metadata ->> 'management_operation_id' = ?", op.SourceTenantID, op.KnowledgeBaseID, op.ID).First(&created).Error
		if err == nil {
			op.NewKnowledgeID = created.ID
			op.NewGeneration = created.ProcessingGeneration
			op.State = OperationStateParsing
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			var input OperationInput
			sourceErr := s.db.WithContext(ctx).Where("operation_id = ?", op.ID).First(&input).Error
			hash := sha256.Sum256(input.Data)
			if sourceErr != nil || hex.EncodeToString(hash[:]) != op.SourceSHA256 {
				fail(fmt.Errorf("cannot recover durable byte-verified source; old document retained: %v", sourceErr))
				return
			}
			var metadata map[string]string
			if len(op.InputMetadata) > 0 {
				if err := json.Unmarshal(op.InputMetadata, &metadata); err != nil {
					fail(err)
					return
				}
			}
			created, createErr := s.createKnowledge(sourceCtx, op.KnowledgeBaseID, input.Data, op.FileName, managedMetadata(metadata, op, op.OldKnowledgeID), op.InputTagIDs)
			if duplicate := duplicateKnowledge(createErr); duplicate != nil {
				// A source from another operation is not adopted for replacement.
				op.NewKnowledgeID = duplicate.ID
				op.State = OperationStateDuplicate
				now := time.Now()
				op.CompletedAt = &now
				op.ResultMessage = "相同文件已存在；旧文档未删除"
				return
			}
			if createErr != nil || created == nil {
				op.ErrorMessage = fmt.Sprintf("recover ingestion: %v", createErr)
				return
			}
			op.NewKnowledgeID = created.ID
			op.NewGeneration = created.ProcessingGeneration
			op.State = OperationStateParsing
		} else {
			op.ErrorMessage = err.Error()
			return
		}
		// Persist the native ingestion identity before a publication transaction
		// can compare-and-swap the parsing phase under this operation's lease.
		return
	}
	knowledge, err := s.knowledgeService.GetKnowledgeByIDOnly(sourceCtx, op.NewKnowledgeID)
	if err != nil || knowledge == nil {
		op.ErrorMessage = "new document unavailable while checking lifecycle"
		return
	}
	if knowledge.TenantID != op.SourceTenantID || knowledge.KnowledgeBaseID != op.KnowledgeBaseID {
		fail(fmt.Errorf("new document identity changed; old document retained"))
		return
	}
	op.EnrichmentStatus = knowledge.EnrichmentStatus
	if op.NewGeneration != "" && op.NewGeneration != knowledge.ProcessingGeneration {
		fail(fmt.Errorf("replacement processing generation changed; old document retained"))
		return
	}
	if knowledge.CoreStatus != types.CoreStatusReady {
		if knowledge.CoreStatus == types.CoreStatusFailed || knowledge.ParseStatus == types.ParseStatusFailed || knowledge.ParseStatus == types.ParseStatusCancelled || knowledge.ParseStatus == types.ParseStatusDeleting {
			fail(fmt.Errorf("new document is not searchable (%s/%s); old document retained", knowledge.CoreStatus, knowledge.ParseStatus))
		}
		return
	}
	op.NewGeneration = knowledge.ProcessingGeneration
	if op.Type == OperationTypeAdd {
		op.Searchable = true
		complete("新文档必要索引已就绪，可检索；可选衍生处理状态单独报告")
		return
	}
	old, err := s.knowledgeService.GetKnowledgeByIDOnly(sourceCtx, op.OldKnowledgeID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		op.ErrorMessage = err.Error()
		return
	}
	if err == nil && old != nil {
		if old.FileHash != op.OldFileHash || old.KnowledgeBaseID != op.KnowledgeBaseID || old.TenantID != op.SourceTenantID {
			fail(fmt.Errorf("old document changed during replacement; both versions retained"))
			return
		}
	}
	// Publish the replacement and retire the old identity in one transaction.
	if err := s.publishReplacement(sourceCtx, op); err != nil {
		if errors.Is(err, ErrPublicationConflict) {
			fail(err)
			return
		}
		op.ErrorMessage = err.Error()
		return
	}
	op.State = OperationStateCleanup
	op.Searchable = true
	op.ResultMessage = "新版本已发布，正在清理旧版本"
	s.cleanup(sourceCtx, op, complete)
}

func (s *Service) cleanup(ctx context.Context, op *Operation, complete func(string)) {
	err := s.knowledgeService.DeleteKnowledge(ctx, op.OldKnowledgeID)
	if err != nil {
		// Native cleanup is idempotent. Missing rows count as complete only
		// when independently confirmed, never for a network/permission error.
		var count int64
		check := s.db.WithContext(ctx).Unscoped().Model(&types.Knowledge{}).Where("id = ? AND tenant_id = ?", op.OldKnowledgeID, op.SourceTenantID).Count(&count).Error
		if check != nil || count != 0 {
			op.State = OperationStateCleanup
			op.ErrorMessage = err.Error()
			return
		}
	}
	var pending int64
	if err := s.db.WithContext(ctx).Model(&types.TaskPendingOp{}).Where("tenant_id = ? AND task_type = ? AND scope_id = ? AND op = ? AND dedup_key = ?", op.SourceTenantID, types.TypeWikiIngest, op.KnowledgeBaseID, "retract", op.OldKnowledgeID).Count(&pending).Error; err != nil {
		op.ErrorMessage = err.Error()
		return
	}
	if pending > 0 {
		op.State = OperationStateCleanup
		op.ResultMessage = "旧文档文件及索引已清理；Wiki 来源撤回仍在执行"
		return
	}
	if op.Type == OperationTypeReplace {
		complete("替换完成：新文档必要索引可用，旧文档及派生资源已清理")
	} else {
		complete("文档及派生资源已清理")
	}
}

func (s *Service) protectReplacement(ctx context.Context, knowledgeID string) error {
	var count int64
	err := s.db.WithContext(ctx).Model(&Operation{}).Where("old_knowledge_id = ? AND type = ? AND state IN ?", strings.TrimSpace(knowledgeID), OperationTypeReplace, []string{OperationStatePreparing, OperationStateParsing, OperationStateCleanup}).Count(&count).Error
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("document belongs to an active replacement; its operation owns cleanup")
	}
	return nil
}
