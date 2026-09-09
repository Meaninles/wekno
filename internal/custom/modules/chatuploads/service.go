// Package chatuploads binds chat originals to private session sources and reuses
// the durable knowledge ingestion pipeline. It owns no second parsing queue.
package chatuploads

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const MaxFileBytes int64 = 128 << 20

var ErrBusy = errors.New("another file is being accepted for this session; retry shortly")
var ErrTooLarge = errors.New("files and images must not exceed 128 MiB per file")

type Service struct {
	db                *gorm.DB
	sessions          interfaces.SessionService
	knowledge         interfaces.KnowledgeService
	kbs               interfaces.KnowledgeBaseService
	models            interfaces.ModelService
	originalInputs    interfaces.FileService
	maintenanceMu     sync.Mutex
	maintenanceCancel context.CancelFunc
	maintenanceDone   chan struct{}
}

func NewService(db *gorm.DB, sessions interfaces.SessionService, knowledge interfaces.KnowledgeService,
	kbs interfaces.KnowledgeBaseService, models interfaces.ModelService, originalInputs ...interfaces.FileService) *Service {
	var originalStorage interfaces.FileService
	if len(originalInputs) > 0 {
		originalStorage = originalInputs[0]
	}
	return &Service{db: db, sessions: sessions, knowledge: knowledge, kbs: kbs, models: models, originalInputs: originalStorage}
}

func privateKBID(sessionID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("weknora/chat-uploads/"+sessionID)).String()
}

func (s *Service) session(ctx context.Context, id string) (*types.Session, error) {
	session, err := s.sessions.GetSession(ctx, id)
	if err != nil || session == nil {
		return nil, fmt.Errorf("chat session not found")
	}
	if session.UserID == "" || session.UserID != types.SessionOwnerIDFromContext(ctx) {
		return nil, fmt.Errorf("chat session owner does not match")
	}
	return session, nil
}

// Acceptance is serialized only for the brief original-file commit. Parsing is
// asynchronous. A process exit releases the database lock automatically; an
// uncertain response is recovered by the same client upload_id in metadata.
func (s *Service) withAcceptance(ctx context.Context, session *types.Session, fn func() error) error {
	if s.db.Dialector.Name() != "postgres" {
		return fmt.Errorf("chat uploads require the PostgreSQL durable runtime")
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("chat-upload:%d:%s", session.TenantID, session.ID)))
	key := int64(binary.BigEndian.Uint64(sum[:8]))
	return s.db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
		var locked bool
		if err := conn.Raw("SELECT pg_try_advisory_lock(?)", key).Scan(&locked).Error; err != nil {
			return err
		}
		if !locked {
			return ErrBusy
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			conn.WithContext(cleanup).Exec("SELECT pg_advisory_unlock(?)", key)
		}()
		return fn()
	})
}

func (s *Service) ensureKB(ctx context.Context, session *types.Session, agent *types.CustomAgent) (*types.KnowledgeBase, error) {
	var kb types.KnowledgeBase
	err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", privateKBID(session.ID), session.TenantID).Take(&kb).Error
	if err == nil {
		if !kb.AllowsPrivateAccess(ctx) || kb.ChatSessionID != session.ID {
			return nil, fmt.Errorf("private source scope mismatch")
		}
		return &kb, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	models, err := s.models.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].IsDefault != models[j].IsDefault {
			return models[i].IsDefault
		}
		return models[i].ID < models[j].ID
	})
	embedding := ""
	for _, model := range models {
		if model.Type == types.ModelTypeEmbedding && model.Status == types.ModelStatusActive {
			embedding = model.ID
			break
		}
	}
	if embedding == "" {
		return nil, fmt.Errorf("an active embedding model is required to process chat files")
	}
	kb = types.KnowledgeBase{ID: privateKBID(session.ID), Name: "对话附件", Type: types.KnowledgeBaseTypeDocument,
		IsTemporary: true, ChatSessionID: session.ID, ChatOwnerID: session.UserID,
		EmbeddingModelID: embedding, IndexingStrategy: types.DefaultIndexingStrategy(),
		ChunkingConfig:           types.ChunkingConfig{ChunkSize: 512, ChunkOverlap: 64, EnableParentChild: true, ParentChunkSize: 4096, ChildChunkSize: 384},
		QuestionGenerationConfig: &types.QuestionGenerationConfig{Enabled: false},
	}
	if agent != nil {
		kb.VLMConfig = types.VLMConfig{Enabled: agent.Config.VLMModelID != "", ModelID: agent.Config.VLMModelID}
		kb.ASRConfig = types.ASRConfig{Enabled: agent.Config.AudioUploadEnabled, ModelID: agent.Config.ASRModelID}
	}
	return s.kbs.CreateKnowledgeBase(ctx, &kb)
}

func (s *Service) Upload(ctx context.Context, sessionID, uploadID string, file *multipart.FileHeader, agent *types.CustomAgent) (*types.Knowledge, error) {
	if _, err := uuid.Parse(uploadID); err != nil {
		return nil, fmt.Errorf("upload_id must be a UUID")
	}
	if file != nil && file.Size > MaxFileBytes {
		return nil, ErrTooLarge
	}
	if file == nil || file.Size <= 0 {
		return nil, fmt.Errorf("file must contain 1 to %d bytes", MaxFileBytes)
	}
	session, err := s.session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	n, hashErr := io.Copy(hash, io.LimitReader(reader, MaxFileBytes+1))
	reader.Close()
	if hashErr != nil {
		return nil, hashErr
	}
	if n != file.Size || n > MaxFileBytes {
		return nil, fmt.Errorf("uploaded file byte count is invalid")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	identity := fileIdentity{file.Filename, file.Size, digest}
	var result *types.Knowledge
	err = s.withAcceptance(ctx, session, func() error {
		if _, err := s.session(ctx, sessionID); err != nil {
			return err
		}
		kb, err := s.ensureKB(ctx, session, agent)
		if err != nil {
			return err
		}
		var existing types.Knowledge
		err = s.db.WithContext(ctx).Where("tenant_id = ? AND knowledge_base_id = ? AND (metadata->>'chat_upload_id' = ? OR jsonb_exists(metadata::jsonb, ?))", session.TenantID, kb.ID, uploadID, bindingPrefix+uploadID).Take(&existing).Error
		if err == nil {
			bound, err := uploadIdentity(&existing, uploadID)
			if err != nil || bound != identity {
				return fmt.Errorf("upload_id is already bound to another file")
			}
			result = &existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		overrides := &types.KnowledgeProcessOverrides{QuestionGenerationConfig: &types.QuestionGenerationConfig{Enabled: false}}
		if agent != nil {
			enabled := agent.Config.VLMModelID != ""
			overrides.EnableMultimodel = &enabled
			overrides.VLMConfig = &types.VLMConfig{Enabled: enabled, ModelID: agent.Config.VLMModelID}
			overrides.ASRConfig = &types.ASRConfig{Enabled: agent.Config.AudioUploadEnabled, ModelID: agent.Config.ASRModelID}
		}
		result, err = s.knowledge.CreateKnowledgeFromFile(ctx, kb.ID, file, map[string]string{"chat_upload_id": uploadID, "chat_upload_sha256": digest}, nil, "", nil, "chat-upload", overrides)
		var duplicate *types.DuplicateKnowledgeError
		if result != nil && errors.As(err, &duplicate) {
			return s.bindDuplicate(ctx, session, result, uploadID, identity)
		}
		return err
	})
	return result, err
}

// HistoryTargets retrieves only files actually sent in this conversation. It
// reads handles in SQL, not full message bodies or all staged uploads. Deleted
// or unpublished sources cannot be resurrected by old attachment metadata.
func (s *Service) HistoryTargets(ctx context.Context, sessionID string) ([]string, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&types.KnowledgeBase{}).Where("id = ? AND tenant_id = ? AND chat_session_id = ?",
		privateKBID(sessionID), types.MustTenantIDFromContext(ctx), sessionID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	session, err := s.session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var ids []string
	err = s.db.WithContext(ctx).Raw(`SELECT DISTINCT k.id FROM messages m
		CROSS JOIN LATERAL jsonb_array_elements(COALESCE(m.attachments, '[]'::jsonb)) a
		JOIN knowledges k ON k.id = a->>'knowledge_id'
		WHERE m.session_id = ? AND m.role = 'user' AND m.deleted_at IS NULL
		AND k.tenant_id = ? AND k.knowledge_base_id = ? AND k.deleted_at IS NULL
		AND k.publication_state = 'published' AND k.enable_status = 'enabled' AND k.published_generation <> ''
		ORDER BY k.id`, sessionID, session.TenantID, privateKBID(sessionID)).Scan(&ids).Error
	return ids, err
}

func (s *Service) List(ctx context.Context, sessionID string) ([]*types.Knowledge, error) {
	session, err := s.session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var rows []*types.Knowledge
	err = s.db.WithContext(ctx).Where("tenant_id = ? AND knowledge_base_id = ?", session.TenantID, privateKBID(session.ID)).Order("created_at DESC, id DESC").Find(&rows).Error
	return rows, err
}

func (s *Service) Get(ctx context.Context, sessionID, id string) (*types.Knowledge, error) {
	session, err := s.session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var row types.Knowledge
	if err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ? AND knowledge_base_id = ?", id, session.TenantID, privateKBID(session.ID)).Take(&row).Error; err != nil {
		return nil, fmt.Errorf("uploaded file not found")
	}
	return &row, nil
}

func ready(k *types.Knowledge) bool {
	return k != nil && k.IsPublished() && k.EnableStatus == "enabled" && k.CoreStatus == types.CoreStatusReady &&
		k.PublishedGeneration != "" && k.PublishedGeneration == k.ProcessingGeneration
}

// Resolve returns handles and retrieval targets. Original bytes and full parsed
// content stay in storage; all agents read through the existing file/RAG tools.
func (s *Service) Resolve(ctx context.Context, sessionID string, ids []string) (types.MessageAttachments, []string, error) {
	if len(ids) > 32 {
		return nil, nil, fmt.Errorf("at most 32 uploaded files may be selected in one turn")
	}
	attachments := make(types.MessageAttachments, 0, len(ids))
	targets := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if seen[id] {
			continue
		}
		seen[id] = true
		k, err := s.Get(ctx, sessionID, id)
		if err != nil {
			return nil, nil, err
		}
		if !ready(k) {
			return nil, nil, fmt.Errorf("file %s is not ready (parse=%s, core=%s)", k.FileName, k.ParseStatus, k.CoreStatus)
		}
		attachments = append(attachments, types.MessageAttachment{UploadID: k.ID, KnowledgeID: k.ID, KnowledgeBaseID: k.KnowledgeBaseID,
			URL: k.FilePath, FileName: k.FileName, FileType: k.FileType, FileSize: k.FileSize})
		targets = append(targets, k.ID)
	}
	return attachments, targets, nil
}
