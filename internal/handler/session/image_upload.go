package session

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

const (
	maxImageSize   = 10 << 20 // 10MB per image
	maxImagesCount = 5
)

// saveImageAttachments decodes base64 images from the request and saves them to
// storage. The images slice is mutated in place: URL is populated.
// Vision interpretation is owned by the unified agent runtime.
func (h *Handler) saveImageAttachments(ctx context.Context, images []ImageAttachment, tenantID uint64, storageProvider string) error {
	return SaveImageAttachments(ctx, h.fileService, images, tenantID, storageProvider)
}

// SaveImageAttachments decodes base64 images from the request and saves them to
// storage. The images slice is mutated in place: URL is populated.
func SaveImageAttachments(ctx context.Context, fileService interfaces.FileService, images []ImageAttachment, tenantID uint64, storageProvider string) error {
	if len(images) == 0 {
		return nil
	}
	if len(images) > maxImagesCount {
		return fmt.Errorf("too many images, max %d", maxImagesCount)
	}

	fileSvc := resolveImageFileService(ctx, fileService, storageProvider)

	for i := range images {
		img := &images[i]
		if img.Data == "" {
			continue
		}

		imgBytes, ext, err := decodeDataURI(img.Data)
		if err != nil {
			return fmt.Errorf("decode image %d: %w", i, err)
		}
		if len(imgBytes) > maxImageSize {
			return fmt.Errorf("image %d too large (%d bytes, max %d)", i, len(imgBytes), maxImageSize)
		}

		storedName := uuid.NewString() + ext
		fileURL, err := fileSvc.SaveBytes(ctx, imgBytes, tenantID, storedName, false)
		if err != nil {
			return fmt.Errorf("save image %d: %w", i, err)
		}
		img.URL = fileURL
	}

	return nil
}

func decodeDataURI(dataURI string) ([]byte, string, error) {
	if !strings.HasPrefix(dataURI, "data:") {
		return nil, "", fmt.Errorf("not a data URI")
	}
	idx := strings.Index(dataURI, ";base64,")
	if idx < 0 {
		return nil, "", fmt.Errorf("unsupported data URI encoding (expected base64)")
	}
	mimeType := dataURI[5:idx]
	decoded, err := base64.StdEncoding.DecodeString(dataURI[idx+8:])
	if err != nil {
		return nil, "", fmt.Errorf("base64 decode: %w", err)
	}
	ext := mimeToExt(mimeType)
	return decoded, ext, nil
}

func mimeToExt(mime string) string {
	switch strings.ToLower(mime) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func (h *Handler) resolveImageFileService(ctx context.Context, storageProvider string) interfaces.FileService {
	return resolveImageFileService(ctx, h.fileService, storageProvider)
}

func resolveImageFileService(ctx context.Context, fileService interfaces.FileService, storageProvider string) interfaces.FileService {
	if strings.TrimSpace(storageProvider) == "" {
		return fileService
	}

	tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	if tenant == nil || tenant.StorageEngineConfig == nil {
		return fileService
	}

	svc, resolvedProvider, err := filesvc.NewFileServiceFromStorageConfig(storageProvider, tenant.StorageEngineConfig, "")
	if err != nil {
		logger.Warnf(ctx, "[image-storage] failed to create %s file service: %v, fallback to default", storageProvider, err)
		return fileService
	}
	logger.Infof(ctx, "[image-storage] using provider=%s for image uploads", resolvedProvider)
	return svc
}
