package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/objectnamespace"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// isValidFileType checks if a file type is supported
func isValidFileType(filename string) bool {
	switch strings.ToLower(getFileType(filename)) {
	case "pdf", "txt", "text", "docx", "doc", "epub", "mhtml", "md", "markdown",
		"png", "jpg", "jpeg", "gif", "webp", "bmp", "tiff",
		"csv", "xlsx", "xls", "pptx", "ppt", "json",
		"mp3", "wav", "m4a", "flac", "ogg":
		return true
	default:
		return false
	}
}

// getFileType extracts the file extension from a filename
func getFileType(filename string) string {
	ext := strings.Split(filename, ".")
	if len(ext) < 2 {
		return "unknown"
	}
	return ext[len(ext)-1]
}

// isValidURL verifies if a URL is valid
// isValidURL 检查URL是否有效
func isValidURL(url string) bool {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return true
	}
	return false
}

// calculateFileHash calculates MD5 hash of a file
func calculateFileHash(file *multipart.FileHeader) (string, error) {
	f, err := file.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	// Reset file pointer for subsequent operations
	if _, err := f.Seek(0, 0); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func calculateStr(strList ...string) string {
	h := md5.New()
	input := strings.Join(strList, "")
	h.Write([]byte(input))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *knowledgeService) getVLMConfig(ctx context.Context, kb *types.KnowledgeBase) (*types.DocParserVLMConfig, error) {
	if kb == nil {
		return nil, nil
	}
	// 兼容老版本：直接使用 ModelName 和 BaseURL
	if kb.VLMConfig.ModelName != "" && kb.VLMConfig.BaseURL != "" {
		return &types.DocParserVLMConfig{
			ModelName:     kb.VLMConfig.ModelName,
			BaseURL:       kb.VLMConfig.BaseURL,
			APIKey:        kb.VLMConfig.APIKey,
			InterfaceType: kb.VLMConfig.InterfaceType,
		}, nil
	}

	// 新版本：未启用或无模型ID时返回nil
	if !kb.VLMConfig.Enabled || kb.VLMConfig.ModelID == "" {
		return nil, nil
	}

	model, err := s.modelService.GetModelByID(ctx, kb.VLMConfig.ModelID)
	if err != nil {
		return nil, err
	}

	interfaceType := model.Parameters.InterfaceType
	if interfaceType == "" {
		interfaceType = "openai"
	}

	return &types.DocParserVLMConfig{
		ModelName:     model.Name,
		BaseURL:       model.Parameters.BaseURL,
		APIKey:        model.Parameters.APIKey,
		InterfaceType: interfaceType,
	}, nil
}

func (s *knowledgeService) buildStorageConfig(ctx context.Context, kb *types.KnowledgeBase) *types.DocParserStorageConfig {
	provider := kb.GetStorageProvider()
	if provider == "" {
		provider = "local"
	}

	// Backward compatibility: if legacy cos_config has full params for the chosen provider, use them.
	// Note: legacy StorageConfig predates tos/s3/oss/ks3, so those providers always
	// resolve via the tenant-merge path below. Listing them here keeps the fall-through
	// intentional (instead of an unrecognised provider silently sliding past the switch).
	// See issue #1117: provider enum was missing tos/s3/oss in this switch.
	sc := &kb.StorageConfig
	hasKBFull := false
	switch provider {
	case "cos":
		hasKBFull = sc.SecretID != "" && sc.BucketName != ""
	case "minio":
		hasKBFull = sc.BucketName != ""
	case "local", "tos", "s3", "oss", "ks3":
		hasKBFull = false
	}

	if hasKBFull {
		logger.Infof(ctx, "[storage] buildStorageConfig use legacy kb config: kb=%s provider=%s bucket=%s path_prefix=%s",
			kb.ID, provider, sc.BucketName, sc.PathPrefix)
		return &types.DocParserStorageConfig{
			Provider:        strings.ToUpper(provider),
			Region:          sc.Region,
			BucketName:      sc.BucketName,
			AccessKeyID:     sc.SecretID,
			SecretAccessKey: sc.SecretKey,
			AppID:           sc.AppID,
			PathPrefix:      sc.PathPrefix,
		}
	}

	// Merge from tenant's StorageEngineConfig.
	var out types.DocParserStorageConfig
	out.Provider = strings.ToUpper(provider)

	tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	if tenant != nil && tenant.StorageEngineConfig != nil {
		sec := tenant.StorageEngineConfig
		if sec.DefaultProvider != "" && provider == "" {
			provider = strings.ToLower(strings.TrimSpace(sec.DefaultProvider))
			out.Provider = strings.ToUpper(provider)
		}
		// Provider list must match types.StorageEngineConfig + ParseProviderScheme.
		// Missing a case here causes DocParserStorageConfig to be returned with only
		// Provider set — bucket/endpoint/credentials are silently dropped, and the
		// docreader then fails or fetches from the wrong location. See issue #1117.
		switch provider {
		case "local":
			if sec.Local != nil {
				out.PathPrefix = sec.Local.PathPrefix
			}
		case "minio":
			if sec.MinIO != nil {
				out.BucketName = sec.MinIO.BucketName
				out.PathPrefix = sec.MinIO.PathPrefix
				if sec.MinIO.Mode == "remote" {
					out.Endpoint = sec.MinIO.Endpoint
					out.AccessKeyID = sec.MinIO.AccessKeyID
					out.SecretAccessKey = sec.MinIO.SecretAccessKey
				} else {
					out.Endpoint = os.Getenv("MINIO_ENDPOINT")
					out.AccessKeyID = os.Getenv("MINIO_ACCESS_KEY_ID")
					out.SecretAccessKey = os.Getenv("MINIO_SECRET_ACCESS_KEY")
				}
				if out.BucketName == "" {
					out.BucketName = os.Getenv("MINIO_BUCKET_NAME")
				}
				if out.PathPrefix == "" && sec.MinIO.Mode != "remote" {
					out.PathPrefix, _ = objectnamespace.KnowledgePrefixFromEnv("minio")
				}
			}
		case "cos":
			if sec.COS != nil {
				out.Region = sec.COS.Region
				out.BucketName = sec.COS.BucketName
				out.AccessKeyID = sec.COS.SecretID
				out.SecretAccessKey = sec.COS.SecretKey
				out.AppID = sec.COS.AppID
				out.PathPrefix = sec.COS.PathPrefix
			}
		case "tos":
			if sec.TOS != nil {
				out.Endpoint = sec.TOS.Endpoint
				out.Region = sec.TOS.Region
				out.AccessKeyID = sec.TOS.AccessKey
				out.SecretAccessKey = sec.TOS.SecretKey
				out.BucketName = sec.TOS.BucketName
				out.PathPrefix = sec.TOS.PathPrefix
			}
		case "s3":
			if sec.S3 != nil {
				out.Endpoint = sec.S3.Endpoint
				out.Region = sec.S3.Region
				out.AccessKeyID = sec.S3.AccessKey
				out.SecretAccessKey = sec.S3.SecretKey
				out.BucketName = sec.S3.BucketName
				out.PathPrefix = sec.S3.PathPrefix
			}
		case "oss":
			if sec.OSS != nil {
				out.Endpoint = sec.OSS.Endpoint
				out.Region = sec.OSS.Region
				out.AccessKeyID = sec.OSS.AccessKey
				out.SecretAccessKey = sec.OSS.SecretKey
				out.BucketName = sec.OSS.BucketName
				out.PathPrefix = sec.OSS.PathPrefix
			}
		case "ks3":
			if sec.KS3 != nil {
				out.Endpoint = sec.KS3.Endpoint
				out.Region = sec.KS3.Region
				out.AccessKeyID = sec.KS3.AccessKey
				out.SecretAccessKey = sec.KS3.SecretKey
				out.BucketName = sec.KS3.BucketName
				out.PathPrefix = sec.KS3.PathPrefix
			}
		case "obs":
			if sec.OBS != nil &&
				strings.TrimSpace(sec.OBS.Endpoint) != "" &&
				strings.TrimSpace(sec.OBS.AccessKey) != "" &&
				strings.TrimSpace(sec.OBS.SecretKey) != "" &&
				strings.TrimSpace(sec.OBS.BucketName) != "" {
				out.Endpoint = sec.OBS.Endpoint
				out.Region = sec.OBS.Region
				out.AccessKeyID = sec.OBS.AccessKey
				out.SecretAccessKey = sec.OBS.SecretKey
				out.BucketName = sec.OBS.BucketName
				out.PathPrefix = sec.OBS.PathPrefix
			} else {
				out.Endpoint = os.Getenv("OBS_ENDPOINT")
				out.Region = os.Getenv("OBS_REGION")
				out.AccessKeyID = os.Getenv("OBS_ACCESS_KEY")
				out.SecretAccessKey = os.Getenv("OBS_SECRET_KEY")
				out.BucketName = os.Getenv("OBS_BUCKET_NAME")
				out.PathPrefix, _ = objectnamespace.KnowledgePrefixFromEnv("obs")
			}
		}
	}

	logger.Infof(ctx, "[storage] buildStorageConfig use merged tenant/global config: kb=%s provider=%s bucket=%s path_prefix=%s endpoint=%s",
		kb.ID, strings.ToLower(out.Provider), out.BucketName, out.PathPrefix, out.Endpoint)
	return &out
}

// resolveFileService returns the FileService for the given knowledge base,
// based on the KB's StorageProviderConfig (or legacy StorageConfig.Provider) and the tenant's StorageEngineConfig.
// Falls back to the global fileSvc when no tenant-level storage config is found.
func (s *knowledgeService) resolveFileService(ctx context.Context, kb *types.KnowledgeBase) interfaces.FileService {
	if kb == nil {
		logger.Infof(ctx, "[storage] resolveFileService fallback default: kb=nil")
		return s.fileSvc
	}

	provider := kb.GetStorageProvider()

	tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	if provider == "" && tenant != nil && tenant.StorageEngineConfig != nil {
		provider = strings.ToLower(strings.TrimSpace(tenant.StorageEngineConfig.DefaultProvider))
	}

	if provider == "" || tenant == nil || tenant.StorageEngineConfig == nil {
		logger.Infof(ctx, "[storage] resolveFileService fallback default: kb=%s provider=%q tenant_cfg=%v",
			kb.ID, provider, tenant != nil && tenant.StorageEngineConfig != nil)
		return s.fileSvc
	}

	sec := tenant.StorageEngineConfig
	baseDir := strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR"))
	svc, resolvedProvider, err := filesvc.NewFileServiceFromStorageConfig(provider, sec, baseDir)
	if err != nil {
		logger.Errorf(ctx, "Failed to create %s file service from tenant config: %v, falling back to default", provider, err)
		return s.fileSvc
	}
	logger.Infof(ctx, "[storage] resolveFileService selected: kb=%s provider=%s", kb.ID, resolvedProvider)
	return svc
}

// resolveFileServiceForPath gives an explicit provider path absolute routing
// precedence. Historical provider:// objects must never be sent to the current
// KB or process-wide default after a storage setting change. Only a path with
// no provider evidence uses the KB's current legacy route.
func (s *knowledgeService) resolveFileServiceForPath(
	ctx context.Context,
	kb *types.KnowledgeBase,
	filePath string,
) (interfaces.FileService, error) {
	svc := s.resolveFileService(ctx, kb)
	if filePath == "" {
		return svc, nil
	}

	inferred := types.InferStorageFromFilePath(filePath)
	if inferred == "" {
		return svc, nil
	}
	tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	if tenant == nil {
		return nil, fmt.Errorf("resolve explicit storage provider %s for %q: tenant is unavailable", inferred, filePath)
	}
	exact, resolvedProvider, err := filesvc.NewFileServiceFromStorageConfig(
		inferred,
		tenant.StorageEngineConfig,
		strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR")),
	)
	if err != nil {
		return nil, fmt.Errorf("resolve explicit storage provider %s for %q: %w", inferred, filePath, err)
	}
	if exact == nil || resolvedProvider != inferred {
		return nil, fmt.Errorf("resolve explicit storage provider %s for %q: resolved as %q", inferred, filePath, resolvedProvider)
	}
	return exact, nil
}

func IsImageType(fileType string) bool {
	switch fileType {
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "svg", "tiff":
		return true
	default:
		return false
	}
}

// IsAudioType checks if a file type is an audio format
func IsAudioType(fileType string) bool {
	switch strings.ToLower(fileType) {
	case "mp3", "wav", "m4a", "flac", "ogg":
		return true
	default:
		return false
	}
}

// IsVideoType checks if a file type is a video format
func IsVideoType(fileType string) bool {
	switch strings.ToLower(fileType) {
	case "mp4", "mov", "avi", "mkv", "webm", "wmv", "flv":
		return true
	default:
		return false
	}
}

type downloadedFile struct {
	File        *os.File
	Path        string
	Size        int64
	MD5         string
	ContentType string
}

func (d *downloadedFile) Close() {
	if d == nil {
		return
	}
	if d.File != nil {
		_ = d.File.Close()
	}
	if d.Path != "" {
		_ = os.Remove(d.Path)
	}
}

// downloadFileFromURL streams a remote file into a seekable temporary file.
// payloadFileName and payloadFileType are in/out pointers: if they point to an empty string,
// the function resolves the value from Content-Disposition / URL path and writes it back.
// It does NOT perform SSRF validation — callers are responsible for that.
func downloadFileFromURL(
	ctx context.Context,
	fileURL string,
	payloadFileName, payloadFileType *string,
) (*downloadedFile, error) {
	if payloadFileName == nil || payloadFileType == nil {
		return nil, errors.New("file URL metadata pointers are required")
	}
	maximum := secutils.GetMaxKnowledgeSourceFileSize()
	httpClient := &http.Client{
		Timeout: 30 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many file URL redirects")
			}
			if err := secutils.ValidateURLForSSRF(req.URL.String()); err != nil {
				return fmt.Errorf("file URL redirect rejected: %w", err)
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for file URL: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download file from URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote server returned status %d", resp.StatusCode)
	}

	// Reject oversized files early via Content-Length
	if contentLength := resp.ContentLength; contentLength > maximum {
		return nil, fmt.Errorf(
			"file size %d bytes exceeds logical document limit of %d bytes",
			contentLength, maximum,
		)
	}

	// Resolve fileName: payload > Content-Disposition > URL path
	if *payloadFileName == "" {
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			*payloadFileName = extractFileNameFromContentDisposition(cd)
		}
	}
	if *payloadFileName == "" {
		*payloadFileName = extractFileNameFromURL(fileURL)
	}
	*payloadFileName = sanitizeDownloadedFileName(*payloadFileName)
	if *payloadFileName == "" {
		return nil, errors.New("remote file name could not be resolved")
	}
	if *payloadFileType == "" && *payloadFileName != "" {
		*payloadFileType = getFileType(*payloadFileName)
	}

	// Stream response body into a temp file, capped at the logical source
	// ceiling. Hashing happens in the same pass; no second in-memory copy is
	// created even for multi-gigabyte sources.
	tmpFile, err := os.CreateTemp("", "weknora-fileurl-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	failed := func(cause error) (*downloadedFile, error) {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return nil, cause
	}
	digest := md5.New()
	limiter := &io.LimitedReader{R: resp.Body, N: maximum + 1}
	written, err := io.Copy(io.MultiWriter(tmpFile, digest), limiter)
	if err != nil {
		return failed(fmt.Errorf("failed to write temp file: %w", err))
	}
	if written > maximum {
		return failed(fmt.Errorf(
			"file size exceeds logical document limit of %d bytes", maximum,
		))
	}
	if err := tmpFile.Sync(); err != nil {
		return failed(fmt.Errorf("flush downloaded file: %w", err))
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return failed(fmt.Errorf("rewind downloaded file: %w", err))
	}
	return &downloadedFile{
		File: tmpFile, Path: tmpPath, Size: written,
		MD5:         hex.EncodeToString(digest.Sum(nil)),
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

func sanitizeDownloadedFileName(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	value = filepath.Base(value)
	if value == "." || value == string(filepath.Separator) {
		return ""
	}
	return value
}
