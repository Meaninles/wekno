package chatuploads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const originalUploadReady = "ready"

// OriginalUpload is the durable handle for an Agent turn's original bytes.
// Unlike types.Knowledge it has no parser, chunk, embedding or publication
// state. The object is private, session-scoped, and removed with the session.
type OriginalUpload struct {
	ID          string         `gorm:"primaryKey;type:varchar(36)"`
	TenantID    uint64         `gorm:"not null;index"`
	SessionID   string         `gorm:"not null;index"`
	OwnerID     string         `gorm:"not null;index"`
	FilePath    string         `gorm:"not null;type:text"`
	FileName    string         `gorm:"not null;type:text"`
	FileType    string         `gorm:"not null;type:varchar(32)"`
	ContentType string         `gorm:"type:varchar(128)"`
	FileSize    int64          `gorm:"not null"`
	SHA256      string         `gorm:"not null;type:varchar(64)"`
	State       string         `gorm:"not null;type:varchar(24);index"`
	CreatedAt   int64          `gorm:"autoCreateTime:nano"`
	UpdatedAt   int64          `gorm:"autoUpdateTime:nano"`
	DeletedAt   gorm.DeletedAt `gorm:"index"`
}

func (OriginalUpload) TableName() string { return "custom_chat_original_uploads" }

func (s *Service) originalUploadStorage() (interfaces.FileService, error) {
	if s.originalInputs == nil {
		return nil, errors.New("Agent 原始文件存储未配置")
	}
	return s.originalInputs, nil
}

func originalUploadFileType(name string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
}

func originalUploadContentType(file *multipart.FileHeader) string {
	if file != nil && file.Header != nil {
		if value := strings.TrimSpace(file.Header.Get("Content-Type")); value != "" {
			return value
		}
	}
	if value := mime.TypeByExtension(strings.ToLower(filepath.Ext(file.Filename))); value != "" {
		return value
	}
	return "application/octet-stream"
}

func originalUploadSource(fileName string) string {
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	if strings.HasPrefix(contentType, "image/") {
		return types.OriginalInputSourceChatImage
	}
	return types.OriginalInputSourceChatUpload
}

func originalUploadIdentity(row *OriginalUpload) fileIdentity {
	return fileIdentity{row.FileName, row.FileSize, row.SHA256}
}

func (s *Service) UploadOriginal(ctx context.Context, sessionID, uploadID string, file *multipart.FileHeader) (*OriginalUpload, error) {
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
	storage, err := s.originalUploadStorage()
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
	var result *OriginalUpload
	err = s.withAcceptance(ctx, session, func() error {
		var existing OriginalUpload
		lookupErr := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ? AND session_id = ? AND deleted_at IS NULL", uploadID, session.TenantID, session.ID).Take(&existing).Error
		if lookupErr == nil {
			if originalUploadIdentity(&existing) != identity {
				return fmt.Errorf("upload_id is already bound to another file")
			}
			result = &existing
			return nil
		}
		if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return lookupErr
		}

		path, saveErr := storage.SaveFile(ctx, file, session.TenantID, "chat-original-"+uploadID)
		if saveErr != nil {
			return saveErr
		}
		row := &OriginalUpload{
			ID: uploadID, TenantID: session.TenantID, SessionID: session.ID, OwnerID: session.UserID,
			FilePath: path, FileName: file.Filename, FileType: originalUploadFileType(file.Filename),
			ContentType: originalUploadContentType(file), FileSize: file.Size, SHA256: digest, State: originalUploadReady,
		}
		if createErr := s.db.WithContext(ctx).Create(row).Error; createErr != nil {
			// The path is known after SaveFile, so a failed row commit can be
			// cleaned up without scanning or deleting another user's object.
			_ = storage.DeleteFile(context.WithoutCancel(ctx), path)
			return createErr
		}
		result = row
		return nil
	})
	return result, err
}

func (s *Service) GetOriginal(ctx context.Context, sessionID, id string) (*OriginalUpload, error) {
	session, err := s.session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var row OriginalUpload
	if err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ? AND session_id = ? AND deleted_at IS NULL", id, session.TenantID, session.ID).Take(&row).Error; err != nil {
		return nil, fmt.Errorf("uploaded original file not found")
	}
	return &row, nil
}

func (s *Service) ListOriginal(ctx context.Context, sessionID string) ([]*OriginalUpload, error) {
	session, err := s.session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var rows []*OriginalUpload
	err = s.db.WithContext(ctx).Where("tenant_id = ? AND session_id = ? AND deleted_at IS NULL", session.TenantID, session.ID).Order("created_at DESC, id DESC").Find(&rows).Error
	return rows, err
}

func (s *Service) OpenOriginal(ctx context.Context, sessionID, id string) (io.ReadCloser, string, string, error) {
	row, err := s.GetOriginal(ctx, sessionID, id)
	if err != nil {
		return nil, "", "", err
	}
	storage, err := s.originalUploadStorage()
	if err != nil {
		return nil, "", "", err
	}
	reader, err := storage.GetFile(ctx, row.FilePath)
	if err != nil {
		return nil, "", "", err
	}
	return reader, row.FileName, row.ContentType, nil
}

func (s *Service) DeleteOriginal(ctx context.Context, sessionID, id string) error {
	row, err := s.GetOriginal(ctx, sessionID, id)
	if err != nil {
		return err
	}
	storage, err := s.originalUploadStorage()
	if err != nil {
		return err
	}
	if err := storage.DeleteFile(ctx, row.FilePath); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Delete(&row).Error
}

func (s *Service) ResolveOriginalInputs(ctx context.Context, sessionID string, ids []string) (types.MessageAttachments, []types.OriginalInputFile, error) {
	if len(ids) > 32 {
		return nil, nil, fmt.Errorf("at most 32 uploaded files may be selected in one turn")
	}
	attachments := make(types.MessageAttachments, 0, len(ids))
	originals := make([]types.OriginalInputFile, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		row, err := s.GetOriginal(ctx, sessionID, id)
		if err != nil {
			return nil, nil, err
		}
		if row.State != originalUploadReady {
			return nil, nil, fmt.Errorf("file %s is not ready", row.FileName)
		}
		storage, err := s.originalUploadStorage()
		if err != nil {
			return nil, nil, err
		}
		downloadURL, err := storage.GetFileURL(ctx, row.FilePath)
		if err != nil {
			return nil, nil, fmt.Errorf("create original file url: %w", err)
		}
		if !isHTTPOriginalURL(downloadURL) {
			return nil, nil, fmt.Errorf("original file storage must provide an HTTP download URL")
		}
		fileType := "." + row.FileType
		attachments = append(attachments, types.MessageAttachment{UploadID: row.ID, URL: row.FilePath, FileName: row.FileName, FileType: fileType, FileSize: row.FileSize})
		originals = append(originals, types.OriginalInputFile{
			ID: row.ID, Source: originalUploadSource(row.FileName), Role: types.OriginalInputRoleUserUploadedOriginal,
			FileName: row.FileName, FileType: fileType, FileSize: row.FileSize, SHA256: row.SHA256,
			DownloadURL: downloadURL, StorageURL: row.FilePath,
		})
	}
	return attachments, originals, nil
}

func isHTTPOriginalURL(value string) bool {
	raw := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}

// HistoryOriginalInputs restores only originals that were persisted on a user
// message. Staged files that were never sent are deliberately excluded.
func (s *Service) HistoryOriginalInputs(ctx context.Context, sessionID string) (types.MessageAttachments, []types.OriginalInputFile, error) {
	session, err := s.session(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	var rows []*OriginalUpload
	err = s.db.WithContext(ctx).Raw(`SELECT o.* FROM custom_chat_original_uploads o
		WHERE o.tenant_id = ? AND o.session_id = ? AND o.deleted_at IS NULL
		AND EXISTS (
			SELECT 1 FROM messages m
			CROSS JOIN LATERAL jsonb_array_elements(COALESCE(m.attachments, '[]'::jsonb)) a
			WHERE m.session_id = o.session_id AND m.role = 'user' AND m.deleted_at IS NULL
			AND a->>'upload_id' = o.id
		)
		ORDER BY o.created_at ASC, o.id ASC`, session.TenantID, session.ID).Scan(&rows).Error
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	// The per-turn selection limit must not limit a conversation's history.
	var originals []types.OriginalInputFile
	for start := 0; start < len(ids); start += 32 {
		_, batch, err := s.ResolveOriginalInputs(ctx, sessionID, ids[start:min(start+32, len(ids))])
		if err != nil {
			return nil, nil, err
		}
		for i := range batch {
			batch[i].Role = "historical_uploaded_file"
		}
		originals = append(originals, batch...)
	}
	return nil, originals, nil
}

func (s *Service) deleteOriginalRow(ctx context.Context, row *OriginalUpload) error {
	if row == nil {
		return nil
	}
	storage, err := s.originalUploadStorage()
	if err != nil {
		return err
	}
	if err := storage.DeleteFile(ctx, row.FilePath); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Delete(row).Error
}
