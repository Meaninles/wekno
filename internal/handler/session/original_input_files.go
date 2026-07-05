package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
)

func (h *Handler) createClaudeOriginalInputFromBytes(
	ctx context.Context,
	data []byte,
	fileName string,
	tenantID uint64,
	source string,
	role string,
) (*types.OriginalInputFile, error) {
	return createClaudeOriginalInputFromBytes(ctx, h.fileService, data, fileName, tenantID, source, role)
}

func createClaudeOriginalInputFromBytes(
	ctx context.Context,
	fileService interfaces.FileService,
	data []byte,
	fileName string,
	tenantID uint64,
	source string,
	role string,
) (*types.OriginalInputFile, error) {
	fileService = resolveClaudeOriginalInputFileService(ctx, fileService)
	if fileService == nil {
		return nil, fmt.Errorf("original file transfer storage is not configured")
	}
	safeName, err := safeOriginalInputFileName(fileName)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	ext := strings.ToLower(filepath.Ext(safeName))
	if ext == "" {
		ext = ".bin"
	}
	transferName := fmt.Sprintf("claude_original_%s%s", uuid.NewString(), ext)
	storageURL, err := fileService.SaveBytes(ctx, data, tenantID, transferName, true)
	if err != nil {
		return nil, fmt.Errorf("save original file transfer object: %w", err)
	}
	downloadURL, err := fileService.GetFileURL(ctx, storageURL)
	if err != nil {
		return nil, fmt.Errorf("create original file download url: %w", err)
	}
	if !isHTTPDownloadURL(downloadURL) {
		return nil, fmt.Errorf("original file transfer requires an HTTP(S) object download URL, got %q", downloadURL)
	}
	return &types.OriginalInputFile{
		ID:          uuid.NewString(),
		Source:      source,
		Role:        role,
		FileName:    safeName,
		FileType:    strings.TrimPrefix(ext, "."),
		FileSize:    int64(len(data)),
		SHA256:      sha,
		DownloadURL: downloadURL,
		StorageURL:  storageURL,
	}, nil
}

func safeOriginalInputFileName(fileName string) (string, error) {
	name := strings.TrimSpace(fileName)
	if name == "" {
		name = "original.bin"
	}
	validated, ok := secutils.ValidateInput(name)
	if !ok {
		return "", fmt.Errorf("invalid original file name")
	}
	safeName, err := secutils.SafeFileName(validated)
	if err != nil {
		return "", fmt.Errorf("unsafe original file name: %w", err)
	}
	if strings.TrimSpace(safeName) == "" {
		return "", fmt.Errorf("empty original file name")
	}
	return safeName, nil
}

func isHTTPDownloadURL(value string) bool {
	raw := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}

func resolveClaudeOriginalInputFileService(ctx context.Context, _ interfaces.FileService) interfaces.FileService {
	provider := claudeOriginalInputStorageProvider()
	if provider == "" {
		return nil
	}
	svc, err := newClaudeOriginalInputFileService(provider)
	if err != nil {
		logger.Warnf(ctx, "[claude-original-input] failed to initialize provider=%s: %v; falling back to existing extracted attachment context", provider, err)
		return nil
	}
	logger.Infof(ctx, "[claude-original-input] using provider=%s", provider)
	return svc
}

// claudeOriginalInputStorageProvider keeps deployment compatibility:
// explicit CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_* config wins, otherwise an
// existing STORAGE_TYPE=obs/minio deployment is reused with the dedicated
// Claude SDK original-input path prefix.
func claudeOriginalInputStorageProvider() string {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_STORAGE_PROVIDER")))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_PROVIDER")))
	}
	if provider != "" {
		return provider
	}
	storageType := strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE_TYPE")))
	if storageType == "obs" || storageType == "minio" {
		return storageType
	}
	return ""
}

func newClaudeOriginalInputFileService(provider string) (interfaces.FileService, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "minio":
		endpoint := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_ENDPOINT"))
		if endpoint == "" {
			endpoint = strings.TrimSpace(os.Getenv("MINIO_ENDPOINT"))
		}
		accessKey := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_ACCESS_KEY_ID"))
		if accessKey == "" {
			accessKey = strings.TrimSpace(os.Getenv("MINIO_ACCESS_KEY_ID"))
		}
		secretKey := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_SECRET_ACCESS_KEY"))
		if secretKey == "" {
			secretKey = strings.TrimSpace(os.Getenv("MINIO_SECRET_ACCESS_KEY"))
		}
		bucket := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_BUCKET"))
		if bucket == "" {
			bucket = strings.TrimSpace(os.Getenv("MINIO_BUCKET_NAME"))
		}
		if bucket == "" {
			bucket = "weknora-original-inputs"
		}
		if endpoint == "" || accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("incomplete minio original input config")
		}
		return filesvc.NewMinioFileServiceWithPathPrefix(
			endpoint,
			accessKey,
			secretKey,
			bucket,
			envBool("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_USE_SSL", false),
			claudeOriginalInputPathPrefix(),
		)
	case "obs":
		endpoint := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_ENDPOINT"))
		if endpoint == "" {
			endpoint = strings.TrimSpace(os.Getenv("OBS_ENDPOINT"))
		}
		region := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_REGION"))
		if region == "" {
			region = strings.TrimSpace(os.Getenv("OBS_REGION"))
		}
		if region == "" {
			region = "cn-north-4"
		}
		accessKey := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_ACCESS_KEY"))
		if accessKey == "" {
			accessKey = strings.TrimSpace(os.Getenv("OBS_ACCESS_KEY"))
		}
		secretKey := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_SECRET_KEY"))
		if secretKey == "" {
			secretKey = strings.TrimSpace(os.Getenv("OBS_SECRET_KEY"))
		}
		bucket := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_BUCKET"))
		if bucket == "" {
			bucket = strings.TrimSpace(os.Getenv("OBS_BUCKET_NAME"))
		}
		if endpoint == "" || accessKey == "" || secretKey == "" || bucket == "" {
			return nil, fmt.Errorf("incomplete obs original input config")
		}
		return filesvc.NewObsFileService(endpoint, region, accessKey, secretKey, bucket, claudeOriginalInputPathPrefix())
	default:
		return nil, fmt.Errorf("unsupported original input provider %q", provider)
	}
}

func claudeOriginalInputPathPrefix() string {
	prefix := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_PATH_PREFIX"))
	if prefix == "" {
		// Keep Claude SDK original-file transfer objects visibly separate from
		// normal uploaded files and knowledge objects.
		prefix = "weknora/__weknora_claude_sdk_original_inputs__/"
	}
	return prefix
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}
