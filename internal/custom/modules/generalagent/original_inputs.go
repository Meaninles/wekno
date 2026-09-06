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
	"time"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/objectnamespace"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

const defaultOriginalInputMaxBytes int64 = 200 * 1024 * 1024

func (s *Service) originalInputFileSpecs(ctx context.Context, req *types.QARequest, runID string) ([]OriginalInputFileSpec, error) {
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
			return nil, fmt.Errorf("prepare selected source %s: %w", knowledgeID, err)
		}
		if spec.ID == "" {
			continue
		} // Non-file entries remain available through retrieval.
		out = append(out, spec)
	}
	if req.Session != nil && req.CustomAgent != nil && req.CustomAgent.Config.MultiTurnEnabled && s.db != nil && s.artifactStore != nil {
		userID := types.SessionOwnerIDFromContext(ctx)
		var rows []Artifact
		err := s.db.WithContext(ctx).Where("tenant_id = ? AND user_id = ? AND session_id = ? AND storage_state = ?", tenantIDFromContext(ctx), userID, req.Session.ID, artifactStorageStateReady).Order("created_at DESC, id DESC").Find(&rows).Error
		if err != nil {
			return nil, fmt.Errorf("load conversation artifact manifest: %w", err)
		} else {
			seen := make(map[string]bool)
			for _, input := range out {
				seen[input.FileName] = true
			}
			for _, row := range rows {
				if seen[row.FileName] {
					continue
				}
				seen[row.FileName] = true
				downloadURL, err := s.artifactStore.DownloadURL(ctx, row.FilePath)
				if err != nil {
					return nil, fmt.Errorf("restore conversation artifact %s: %w", row.ID, err)
				}
				// StorageURL is intentionally absent: this is a persistent delivered
				// version, not a disposable original-input transfer object.
				out = append(out, OriginalInputFileSpec{ID: row.ID, Source: "conversation_artifact", Role: "previously_delivered_version", FileName: row.FileName, FileType: row.FileType, FileSize: row.FileSize, SHA256: row.SHA256, DownloadURL: downloadURL})
			}
		}
	}
	return out, nil
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
	if knowledge != nil && knowledge.Type != "file" {
		return OriginalInputFileSpec{}, nil
	}
	if knowledge == nil || strings.TrimSpace(knowledge.FilePath) == "" {
		return OriginalInputFileSpec{}, fmt.Errorf("选中的知识库条目不是可传给 Claude SDK 的原始文件：%s", knowledgeID)
	}
	if knowledge.FileSize <= 0 || knowledge.FileSize > originalInputMaxBytes() {
		return OriginalInputFileSpec{}, fmt.Errorf("original file size is outside the configured limit")
	}
	// Hash directly from storage without allocating or uploading another copy.
	// Chat-private files already carry the server-computed acceptance hash.
	sha := ""
	var kb types.KnowledgeBase
	if err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", knowledge.KnowledgeBaseID, knowledge.TenantID).Take(&kb).Error; err != nil {
		return OriginalInputFileSpec{}, err
	}
	if kb.ChatSessionID != "" && kb.AllowsPrivateAccess(ctx) {
		sha = knowledge.GetMetadata()["chat_upload_sha256"]
	}
	if len(sha) != 64 {
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(reader, originalInputMaxBytes()+1))
		if err != nil {
			return OriginalInputFileSpec{}, err
		}
		if n != knowledge.FileSize {
			return OriginalInputFileSpec{}, fmt.Errorf("original file size mismatch: expected %d, read %d", knowledge.FileSize, n)
		}
		sha = hex.EncodeToString(hash.Sum(nil))
	}
	storage, err := knowledgeaux.New(s.db, s.fileService).SourceFileServiceForRead(ctx, knowledge.TenantID, knowledge.KnowledgeBaseID, knowledge.ID, knowledge.FilePath, kb.GetStorageProvider())
	if err != nil {
		return OriginalInputFileSpec{}, err
	}
	downloadURL, err := storage.GetFileURL(ctx, knowledge.FilePath)
	if err != nil {
		return OriginalInputFileSpec{}, err
	}
	if !isHTTPDownloadURL(downloadURL) {
		return OriginalInputFileSpec{}, fmt.Errorf("original source requires an HTTP download URL")
	}
	if strings.TrimSpace(filename) == "" {
		filename = knowledge.FileName
	}
	name, err := safeOriginalInputFileName(filename)
	if err != nil {
		return OriginalInputFileSpec{}, err
	}
	// No StorageURL: the original belongs to its knowledge lifecycle and must
	// never be removed by the run's disposable transfer cleanup.
	return OriginalInputFileSpec{ID: knowledge.ID, Source: types.OriginalInputSourceSelectedKnowledge,
		Role: types.OriginalInputRoleSelectedKnowledgeOriginal, FileName: name,
		FileType: strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), "."), FileSize: knowledge.FileSize,
		SHA256: sha, DownloadURL: downloadURL, KnowledgeID: knowledge.ID, KnowledgeBaseID: knowledge.KnowledgeBaseID}, nil
}

func cleanupOriginalInputTransferObject(
	ctx context.Context,
	fileService interfaces.FileService,
	storageURL string,
) {
	if fileService == nil || strings.TrimSpace(storageURL) == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := fileService.DeleteFile(cleanupCtx, storageURL); err != nil {
		logger.Warnf(cleanupCtx, "[claude-original-input] cleanup failed: %v", err)
	}
}

func (s *Service) cleanupOriginalInputTransferObjects(
	ctx context.Context,
	storageURLs map[string]struct{},
) {
	if len(storageURLs) == 0 {
		return
	}
	fileService := resolveOriginalInputFileService(ctx, s.fileService)
	if fileService == nil {
		logger.Warnf(ctx, "[claude-original-input] cleanup skipped because transfer storage is unavailable")
		return
	}
	for storageURL := range storageURLs {
		cleanupOriginalInputTransferObject(ctx, fileService, storageURL)
	}
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
	pathPrefix, err := objectnamespace.NormalizeAndValidate(
		originalInputPathPrefix(),
		objectnamespace.PurposeOriginalInputs,
	)
	if err != nil {
		return nil, err
	}
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
			pathPrefix,
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
		return filesvc.NewObsFileService(endpoint, region, accessKey, secretKey, bucket, pathPrefix)
	default:
		return nil, fmt.Errorf("unsupported original input provider %q", provider)
	}
}

func originalInputPathPrefix() string {
	return strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_PATH_PREFIX"))
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
