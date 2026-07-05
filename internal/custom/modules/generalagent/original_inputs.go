package generalagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

const defaultOriginalInputMaxBytes int64 = 200 * 1024 * 1024

func (s *Service) originalInputFileSpecs(ctx context.Context, req *types.QARequest, runID string) []OriginalInputFileSpec {
	out := make([]OriginalInputFileSpec, 0, len(req.OriginalInputFiles)+len(req.KnowledgeIDs))
	for _, item := range req.OriginalInputFiles {
		spec, err := originalInputFileSpecFromRuntime(item)
		if err != nil {
			logger.Warnf(ctx, "skip invalid runtime original input file %s: %v", item.FileName, err)
			continue
		}
		out = append(out, spec)
	}

	// Only explicitly selected knowledge files use the original-file materialization
	// path. Selected knowledge bases stay on the existing retrieval/tool path.
	for _, knowledgeID := range compactStrings(req.KnowledgeIDs) {
		spec, err := s.selectedKnowledgeOriginalInputSpec(ctx, knowledgeID, runID)
		if err != nil {
			logger.Warnf(ctx, "selected knowledge original input preparation failed for %s, falling back to existing knowledge context/tools: %v", knowledgeID, err)
			continue
		}
		out = append(out, spec)
	}
	return out
}

func originalInputFileSpecFromRuntime(item types.OriginalInputFile) (OriginalInputFileSpec, error) {
	if strings.TrimSpace(item.DownloadURL) == "" {
		return OriginalInputFileSpec{}, fmt.Errorf("original input file %s is missing download url", item.FileName)
	}
	if strings.TrimSpace(item.SHA256) == "" || item.FileSize < 0 {
		return OriginalInputFileSpec{}, fmt.Errorf("original input file %s is missing verification metadata", item.FileName)
	}
	return OriginalInputFileSpec{
		ID:              item.ID,
		Source:          item.Source,
		Role:            item.Role,
		FileName:        item.FileName,
		FileType:        strings.TrimPrefix(strings.ToLower(item.FileType), "."),
		FileSize:        item.FileSize,
		SHA256:          item.SHA256,
		DownloadURL:     item.DownloadURL,
		StorageURL:      item.StorageURL,
		KnowledgeID:     item.KnowledgeID,
		KnowledgeBaseID: item.KnowledgeBaseID,
	}, nil
}

func (s *Service) selectedKnowledgeOriginalInputSpec(
	ctx context.Context,
	knowledgeID string,
	runID string,
) (OriginalInputFileSpec, error) {
	if s.knowledgeService == nil {
		return OriginalInputFileSpec{}, fmt.Errorf("通用智能体无法读取选中的知识库原文件：知识库服务未初始化")
	}
	if s.fileService == nil {
		return OriginalInputFileSpec{}, fmt.Errorf("通用智能体无法准备选中的知识库原文件：对象存储未初始化")
	}

	reader, filename, knowledge, err := s.knowledgeService.GetKnowledgeFileWithSharedAccess(ctx, tenantIDFromContext(ctx), knowledgeID)
	if err != nil {
		return OriginalInputFileSpec{}, fmt.Errorf("读取选中的知识库原文件失败（%s）: %w", knowledgeID, err)
	}
	defer reader.Close()
	if knowledge == nil || knowledge.Type != "file" || strings.TrimSpace(knowledge.FilePath) == "" {
		return OriginalInputFileSpec{}, fmt.Errorf("选中的知识库条目不是可传给 Claude SDK 的原始文件：%s", knowledgeID)
	}
	data, err := readOriginalInputBytes(reader, originalInputMaxBytes())
	if err != nil {
		return OriginalInputFileSpec{}, fmt.Errorf("读取选中的知识库原文件内容失败（%s）: %w", knowledgeID, err)
	}
	if knowledge.FileSize > 0 && int64(len(data)) != knowledge.FileSize {
		return OriginalInputFileSpec{}, fmt.Errorf(
			"选中的知识库原文件大小校验失败（%s）：记录=%d 实际=%d",
			knowledgeID,
			knowledge.FileSize,
			len(data),
		)
	}
	if strings.TrimSpace(filename) == "" {
		filename = knowledge.FileName
	}
	runtimeFile, err := s.createOriginalInputTransferObject(
		ctx,
		data,
		filename,
		tenantIDFromContext(ctx),
		types.OriginalInputSourceSelectedKnowledge,
		types.OriginalInputRoleSelectedKnowledgeOriginal,
	)
	if err != nil {
		return OriginalInputFileSpec{}, fmt.Errorf("准备选中的知识库原文件传输对象失败（%s）: %w", knowledgeID, err)
	}
	runtimeFile.KnowledgeID = knowledge.ID
	runtimeFile.KnowledgeBaseID = knowledge.KnowledgeBaseID
	return originalInputFileSpecFromRuntime(*runtimeFile)
}

func (s *Service) createOriginalInputTransferObject(
	ctx context.Context,
	data []byte,
	fileName string,
	tenantID uint64,
	source string,
	role string,
) (*types.OriginalInputFile, error) {
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
	fileService := resolveOriginalInputFileService(ctx, s.fileService)
	if fileService == nil {
		return nil, fmt.Errorf("original file transfer storage is not configured")
	}
	transferName := fmt.Sprintf("claude_original_%s%s", uuid.NewString(), ext)
	storageURL, err := fileService.SaveBytes(ctx, data, tenantID, transferName, true)
	if err != nil {
		return nil, err
	}
	downloadURL, err := fileService.GetFileURL(ctx, storageURL)
	if err != nil {
		return nil, err
	}
	if !isHTTPDownloadURL(downloadURL) {
		return nil, fmt.Errorf("Claude SDK 原文件传输需要 HTTP(S) 对象下载 URL，当前得到 %q", downloadURL)
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

func readOriginalInputBytes(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = defaultOriginalInputMaxBytes
	}
	limited := io.LimitReader(reader, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds original input limit of %.1fMB", float64(maxBytes)/1024/1024)
	}
	return data, nil
}

func originalInputMaxBytes() int64 {
	mb := envInt("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MAX_MB", int(defaultOriginalInputMaxBytes/1024/1024))
	return int64(mb) * 1024 * 1024
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
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

func resolveOriginalInputFileService(ctx context.Context, _ interfaces.FileService) interfaces.FileService {
	provider := originalInputStorageProvider()
	if provider == "" {
		return nil
	}
	svc, err := newOriginalInputFileService(provider)
	if err != nil {
		logger.Warnf(ctx, "[claude-original-input] failed to initialize provider=%s: %v; falling back to existing knowledge context/tools", provider, err)
		return nil
	}
	logger.Infof(ctx, "[claude-original-input] using provider=%s", provider)
	return svc
}

// originalInputStorageProvider keeps deployment compatibility:
// explicit CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_* config wins, otherwise an
// existing STORAGE_TYPE=obs/minio deployment is reused with the dedicated
// Claude SDK original-input path prefix.
func originalInputStorageProvider() string {
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

func newOriginalInputFileService(provider string) (interfaces.FileService, error) {
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
			originalInputPathPrefix(),
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
		return filesvc.NewObsFileService(endpoint, region, accessKey, secretKey, bucket, originalInputPathPrefix())
	default:
		return nil, fmt.Errorf("unsupported original input provider %q", provider)
	}
}

func originalInputPathPrefix() string {
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
