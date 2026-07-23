package file

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"path"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

// DummyFileService is a no-op implementation of the FileService interface
// used for testing or when file storage is not required
type DummyFileService struct {
	mu            sync.RWMutex
	objects       map[string][]byte
	bindingSource string
}

const dummyScheme = "dummy://"

var _ interfaces.PlannedFileService = (*DummyFileService)(nil)

// CheckConnectivity always succeeds for the dummy service.
func (s *DummyFileService) CheckConnectivity(ctx context.Context) error {
	return nil
}

// NewDummyFileService creates a new instance of DummyFileService
func NewDummyFileService() interfaces.FileService {
	return &DummyFileService{objects: make(map[string][]byte), bindingSource: "direct"}
}

func (s *DummyFileService) ReserveFilePath(
	tenantID uint64, knowledgeID string, fileName string,
) (string, error) {
	key, err := plannedfile.FileKey("", tenantID, knowledgeID, fileName)
	if err != nil {
		return "", err
	}
	return dummyScheme + key, nil
}

func (s *DummyFileService) ReserveBytesPath(
	tenantID uint64, fileName string, temp bool,
) (string, error) {
	layout := "exports"
	if temp {
		layout = "temp"
	}
	key, err := plannedfile.BytesKey("", tenantID, fileName, layout)
	if err != nil {
		return "", err
	}
	return dummyScheme + key, nil
}

func (s *DummyFileService) ReserveCopyPath(
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	sourceKey, err := parseDummyPlannedPath(srcPath)
	if err != nil {
		return "", fmt.Errorf("dummy copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}
	return s.ReserveFilePath(tenantID, knowledgeID, "copy"+path.Ext(sourceKey))
}

func (s *DummyFileService) CommitFileAtPath(
	ctx context.Context, file *multipart.FileHeader, filePath string,
) error {
	if file == nil {
		return errors.New("planned dummy commit: file is nil")
	}
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("planned dummy commit: open upload: %w", err)
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		return fmt.Errorf("planned dummy commit: read upload: %w", err)
	}
	return s.CommitBytesAtPath(ctx, data, filePath)
}

func (s *DummyFileService) commitReaderAtPath(
	ctx context.Context, reader io.ReadSeeker, _ int64, _ string, filePath string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := parseDummyPlannedPath(filePath)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.objects[key] = data
	s.mu.Unlock()
	return nil
}

func (s *DummyFileService) CommitBytesAtPath(ctx context.Context, data []byte, filePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := parseDummyPlannedPath(filePath); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = make(map[string][]byte)
	}
	s.objects[filePath] = append([]byte(nil), data...)
	return nil
}

func (s *DummyFileService) CommitCopyAtPath(ctx context.Context, srcPath string, dstPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := parseDummyPlannedPath(srcPath); err != nil {
		return fmt.Errorf("planned dummy copy source: %w", err)
	}
	if _, err := parseDummyPlannedPath(dstPath); err != nil {
		return fmt.Errorf("planned dummy copy destination: %w", err)
	}
	s.mu.RLock()
	data, ok := s.objects[srcPath]
	data = append([]byte(nil), data...)
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("planned dummy copy source does not exist")
	}
	return s.CommitBytesAtPath(ctx, data, dstPath)
}

func parseDummyPlannedPath(filePath string) (string, error) {
	if !strings.HasPrefix(filePath, dummyScheme) {
		return "", fmt.Errorf("planned dummy path provider mismatch")
	}
	key := strings.TrimPrefix(filePath, dummyScheme)
	if err := plannedfile.ValidateKey(key, ""); err != nil {
		return "", err
	}
	return key, nil
}

// SaveFile pretends to save a file but just returns a random UUID
// This is useful for testing without actual file operations
func (s *DummyFileService) SaveFile(ctx context.Context,
	file *multipart.FileHeader, tenantID uint64, knowledgeID string,
) (string, error) {
	return uuid.New().String(), nil
}

// GetFile always returns an error as dummy service doesn't store files
func (s *DummyFileService) GetFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	s.mu.RLock()
	data, ok := s.objects[filePath]
	data = append([]byte(nil), data...)
	s.mu.RUnlock()
	if ok {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return nil, errors.New("not implemented")
}

// DeleteFile is a no-op operation that always succeeds
func (s *DummyFileService) DeleteFile(ctx context.Context, filePath string) error {
	if strings.HasPrefix(filePath, dummyScheme) {
		if _, err := parseDummyPlannedPath(filePath); err != nil {
			return err
		}
		s.mu.Lock()
		delete(s.objects, filePath)
		s.mu.Unlock()
	}
	return nil
}

// SaveBytes pretends to save bytes but just returns a random UUID
func (s *DummyFileService) SaveBytes(ctx context.Context, data []byte, tenantID uint64, fileName string, temp bool) (string, error) {
	return uuid.New().String(), nil
}

// CopyFile is a no-op for the dummy service: it logs a warning and returns the
// source path unchanged (the shared reference is intentional in this stub).
func (s *DummyFileService) CopyFile(ctx context.Context, srcPath string, tenantID uint64, knowledgeID string) (string, error) {
	logger.Warnf(ctx, "[dummy] CopyFile no-op: returning source path %q unchanged (no real copy performed)", srcPath)
	return srcPath, nil
}

// GetFileURL returns the file path as URL (dummy implementation)
func (s *DummyFileService) GetFileURL(ctx context.Context, filePath string) (string, error) {
	return filePath, nil
}
