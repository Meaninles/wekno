package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/tencentyun/cos-go-sdk-v5"
)

// cosFileService implements the FileService interface for Tencent Cloud COS
type cosFileService struct {
	client          *cos.Client
	bucketURL       string
	cosPathPrefix   string
	tempClient      *cos.Client
	tempBucketURL   string
	tempBucketName  string
	tempRegion      string
	bucketName      string
	region          string
	bindingSource   string
	credentialScope string
	credentialRef   string
}

const (
	cosProvider = "cos"
	cosScheme   = cosProvider + "://"
)

var _ interfaces.PlannedFileService = (*cosFileService)(nil)

// newCosClient creates a bare cosFileService with just the SDK client initialised.
// Shared by NewCosFileService* constructors and CheckCosConnectivity.
func newCosClient(bucketName, region, secretID, secretKey string) (*cosFileService, error) {
	bucketURL := fmt.Sprintf("https://%s.cos.%s.myqcloud.com/", bucketName, region)
	u, err := url.Parse(bucketURL)
	logger.Infof(context.Background(), "newCosClient: bucketURL: %s", bucketURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse bucketURL: %w", err)
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: u}, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  secretID,
			SecretKey: secretKey,
		},
	})
	credentialRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeDirect, storagebinding.ProviderCOS, "default",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to identify COS credentials: %w", err)
	}
	return &cosFileService{
		client: client, bucketURL: bucketURL, bucketName: bucketName, region: region,
		bindingSource:   "direct",
		credentialScope: "direct",
		credentialRef:   credentialRef,
	}, nil
}

// NewCosFileService creates a new COS file service instance
func NewCosFileService(bucketName, region, secretId, secretKey, cosPathPrefix string) (interfaces.FileService, error) {
	return NewCosFileServiceWithTempBucket(bucketName, region, secretId, secretKey, cosPathPrefix, "", "")
}

// NewCosFileServiceWithTempBucket creates a new COS file service instance with optional temp bucket
func NewCosFileServiceWithTempBucket(bucketName, region, secretId, secretKey, cosPathPrefix, tempBucketName, tempRegion string) (interfaces.FileService, error) {
	svc, err := newCosClient(bucketName, region, secretId, secretKey)
	if err != nil {
		return nil, err
	}
	svc.cosPathPrefix = cosPathPrefix

	if tempBucketName != "" {
		if tempRegion == "" {
			tempRegion = region
		}
		tempBucketURL := fmt.Sprintf("https://%s.cos.%s.myqcloud.com/", tempBucketName, tempRegion)
		tempU, err := url.Parse(tempBucketURL)
		if err != nil {
			return nil, fmt.Errorf("failed to parse temp bucketURL: %w", err)
		}
		svc.tempClient = cos.NewClient(&cos.BaseURL{BucketURL: tempU}, &http.Client{
			Transport: &cos.AuthorizationTransport{
				SecretID:  secretId,
				SecretKey: secretKey,
			},
		})
		svc.tempBucketURL = tempBucketURL
		svc.tempBucketName = tempBucketName
		svc.tempRegion = tempRegion
	}

	return svc, nil
}

func (s *cosFileService) ReserveFilePath(
	tenantID uint64, knowledgeID string, fileName string,
) (string, error) {
	key, err := plannedfile.FileKey(s.cosPathPrefix, tenantID, knowledgeID, fileName)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatRegionPath(cosProvider, s.bucketName, s.region, key)
}

func (s *cosFileService) ReserveBytesPath(
	tenantID uint64, fileName string, temp bool,
) (string, error) {
	if temp && s.tempClient != nil {
		name, err := plannedfile.NewObjectName(fileName)
		if err != nil {
			return "", err
		}
		key, err := plannedfile.BuildKey("", "exports", strconv.FormatUint(tenantID, 10), name)
		if err != nil {
			return "", err
		}
		return plannedfile.FormatRegionPath(cosProvider, s.tempBucketName, s.tempRegion, key)
	}
	key, err := plannedfile.BytesKey(s.cosPathPrefix, tenantID, fileName, "exports")
	if err != nil {
		return "", err
	}
	return plannedfile.FormatRegionPath(cosProvider, s.bucketName, s.region, key)
}

func (s *cosFileService) ReserveCopyPath(
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	srcKey, err := plannedfile.ParseRegionPath(srcPath, cosProvider, s.bucketName, s.region, "")
	if err != nil {
		return "", fmt.Errorf("cos copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}
	return s.ReserveFilePath(tenantID, knowledgeID, "copy"+path.Ext(srcKey))
}

func (s *cosFileService) plannedMainKey(filePath string) (string, error) {
	key, err := plannedfile.ParseRegionPath(
		filePath, cosProvider, s.bucketName, s.region, s.cosPathPrefix,
	)
	if err != nil {
		return "", fmt.Errorf("planned COS destination is not bound to this service: %w", err)
	}
	return key, nil
}

func (s *cosFileService) plannedBytesTarget(filePath string) (*cos.Client, string, error) {
	if key, err := s.plannedMainKey(filePath); err == nil {
		return s.client, key, nil
	}
	if s.tempClient != nil {
		key, err := plannedfile.ParseRegionPath(
			filePath, cosProvider, s.tempBucketName, s.tempRegion, "exports",
		)
		if err == nil {
			return s.tempClient, key, nil
		}
	}
	return nil, "", fmt.Errorf("planned COS bytes destination is not bound to this service")
}

func (s *cosFileService) CommitFileAtPath(
	ctx context.Context, file *multipart.FileHeader, filePath string,
) error {
	if file == nil {
		return fmt.Errorf("planned COS commit: file is nil")
	}
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("planned COS commit: open upload: %w", err)
	}
	defer src.Close()
	if _, err := s.client.Object.Put(ctx, key, src, nil); err != nil {
		return fmt.Errorf("planned COS commit: upload: %w", err)
	}
	return nil
}

func (s *cosFileService) commitReaderAtPath(
	ctx context.Context, reader io.ReadSeeker, _ int64, _ string, filePath string,
) error {
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	if _, err := s.client.Object.Put(ctx, key, reader, nil); err != nil {
		return fmt.Errorf("planned COS stream commit: %w", err)
	}
	return nil
}

func (s *cosFileService) CommitBytesAtPath(ctx context.Context, data []byte, filePath string) error {
	client, key, err := s.plannedBytesTarget(filePath)
	if err != nil {
		return err
	}
	if _, err := client.Object.Put(ctx, key, bytes.NewReader(data), nil); err != nil {
		return fmt.Errorf("planned COS bytes commit: upload: %w", err)
	}
	return nil
}

func (s *cosFileService) CommitCopyAtPath(ctx context.Context, srcPath string, dstPath string) error {
	srcKey, err := plannedfile.ParseRegionPath(srcPath, cosProvider, s.bucketName, s.region, "")
	if err != nil {
		return fmt.Errorf("planned COS copy source: %w", err)
	}
	dstKey, err := s.plannedMainKey(dstPath)
	if err != nil {
		return err
	}
	sourceURL := fmt.Sprintf("%s.cos.%s.myqcloud.com/%s", s.bucketName, s.region, srcKey)
	if _, _, err := s.client.Object.Copy(ctx, dstKey, sourceURL, nil); err != nil {
		return fmt.Errorf("planned COS copy commit: %w", err)
	}
	return nil
}

// CheckConnectivity verifies COS is reachable by performing a HEAD request on the bucket.
func (s *cosFileService) CheckConnectivity(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.client.Bucket.Head(checkCtx)
	return err
}

// CheckCosConnectivity tests COS connectivity using the provided credentials.
// It creates a temporary service instance internally and delegates to CheckConnectivity.
func CheckCosConnectivity(ctx context.Context, bucketName, region, secretID, secretKey string) error {
	svc, err := newCosClient(bucketName, region, secretID, secretKey)
	if err != nil {
		return err
	}
	return svc.CheckConnectivity(ctx)
}

// SaveFile saves a file to COS storage
// It generates a unique name for the file and organizes it by tenant and knowledge ID
func (s *cosFileService) SaveFile(ctx context.Context,
	file *multipart.FileHeader, tenantID uint64, knowledgeID string,
) (string, error) {
	if file == nil {
		return "", fmt.Errorf("failed to save file to COS: file is nil")
	}
	filePath, err := s.ReserveFilePath(tenantID, knowledgeID, file.Filename)
	if err != nil {
		return "", err
	}
	if err := s.CommitFileAtPath(ctx, file, filePath); err != nil {
		return "", err
	}
	return filePath, nil
}

// GetFile retrieves a file from COS storage by its path URL
func (s *cosFileService) GetFile(ctx context.Context, filePathUrl string) (io.ReadCloser, error) {
	client, objectName, err := s.resolveCosObject(filePathUrl)
	if err != nil {
		return nil, err
	}
	if err := utils.SafeObjectKey(objectName); err != nil {
		return nil, fmt.Errorf("invalid file path: %w", err)
	}
	resp, err := client.Object.Get(ctx, objectName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get file from COS: %w", err)
	}
	return resp.Body, nil
}

// DeleteFile removes a file from COS storage
func (s *cosFileService) DeleteFile(ctx context.Context, filePath string) error {
	client, objectName, err := s.resolveCosObject(filePath)
	if err != nil {
		return err
	}
	if err := utils.SafeObjectKey(objectName); err != nil {
		return fmt.Errorf("invalid file path: %w", err)
	}
	_, err = client.Object.Delete(ctx, objectName)
	if err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}
	return nil
}

// parseCosObjectName extracts the object name from:
// - provider scheme: cos://{bucket}/{region}/{objectKey}
// - legacy URL: https://bucket.cos.region.myqcloud.com/{objectKey}
func (s *cosFileService) parseCosObjectName(filePath string) (string, error) {
	for _, other := range []string{"local://", "minio://", "s3://", "tos://", "oss://", "ks3://", "obs://"} {
		if strings.HasPrefix(filePath, other) {
			return "", fmt.Errorf("cos file service cannot resolve %s path", strings.Split(other, "://")[0])
		}
	}
	// Provider scheme format: cos://{bucket}/{region}/{objectKey}
	if strings.HasPrefix(filePath, cosScheme) {
		rest := strings.TrimPrefix(filePath, cosScheme)
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) == 3 {
			return parts[2], nil
		}
		return rest, nil
	}
	// Legacy format: https://bucket.cos.region.myqcloud.com/{objectKey}
	return strings.TrimPrefix(filePath, s.bucketURL), nil
}

// resolveCosObject retains legacy HTTPS reads while binding every canonical
// cos:// path to this service's configured main or temporary bucket and region.
func (s *cosFileService) resolveCosObject(filePath string) (*cos.Client, string, error) {
	if strings.HasPrefix(filePath, cosScheme) {
		if key, err := plannedfile.ParseRegionPath(
			filePath, cosProvider, s.bucketName, s.region, "",
		); err == nil {
			return s.client, key, nil
		}
		if s.tempClient != nil {
			if key, err := plannedfile.ParseRegionPath(
				filePath, cosProvider, s.tempBucketName, s.tempRegion, "",
			); err == nil {
				return s.tempClient, key, nil
			}
		}
		return nil, "", fmt.Errorf("COS path is not bound to this service")
	}
	if s.tempClient != nil && strings.HasPrefix(filePath, s.tempBucketURL) {
		key := strings.TrimPrefix(filePath, s.tempBucketURL)
		if err := plannedfile.ValidateKey(key, ""); err != nil {
			return nil, "", fmt.Errorf("invalid COS temp path: %w", err)
		}
		return s.tempClient, key, nil
	}
	// Legacy HTTPS paths are accepted only when their complete bucket URL is
	// exactly the one configured on this service.  Merely trimming a non-matching
	// prefix would turn a URL for another account/bucket into a local object key.
	if !strings.HasPrefix(filePath, s.bucketURL) {
		return nil, "", fmt.Errorf("COS path is not bound to this service")
	}
	key := strings.TrimPrefix(filePath, s.bucketURL)
	if err := plannedfile.ValidateKey(key, ""); err != nil {
		return nil, "", fmt.Errorf("invalid COS path: %w", err)
	}
	return s.client, key, nil
}

// CopyFile copies an existing COS object to a new knowledge-owned object using a
// server-side Object.Copy (no data leaves COS). The destination uses the same
// layout as SaveFile. Returns ErrCrossBackendCopy when srcPath is not a cos:// path.
func (s *cosFileService) CopyFile(ctx context.Context,
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	canonicalSource := srcPath
	if !strings.HasPrefix(srcPath, cosScheme) {
		if !strings.HasPrefix(srcPath, s.bucketURL) {
			return "", fmt.Errorf("cos copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
		}
		key := strings.TrimPrefix(srcPath, s.bucketURL)
		if err := plannedfile.ValidateKey(key, ""); err != nil {
			return "", fmt.Errorf("cos copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
		}
		var err error
		canonicalSource, err = plannedfile.FormatRegionPath(
			cosProvider, s.bucketName, s.region, key,
		)
		if err != nil {
			return "", err
		}
	}
	newPath, err := s.ReserveCopyPath(canonicalSource, tenantID, knowledgeID)
	if err != nil {
		return "", err
	}
	if err := s.CommitCopyAtPath(ctx, canonicalSource, newPath); err != nil {
		return "", err
	}
	logger.Infof(ctx, "Copied COS object %s to %s", srcPath, newPath)
	return newPath, nil
}

// SaveBytes saves bytes data to COS
// If temp is true and temp bucket is configured, saves to temp bucket (with lifecycle auto-expiration)
// Otherwise saves to main bucket
func (s *cosFileService) SaveBytes(ctx context.Context, data []byte, tenantID uint64, fileName string, temp bool) (string, error) {
	filePath, err := s.ReserveBytesPath(tenantID, fileName, temp)
	if err != nil {
		return "", err
	}
	if err := s.CommitBytesAtPath(ctx, data, filePath); err != nil {
		return "", err
	}
	if temp && s.tempClient != nil {
		key, err := plannedfile.ParseRegionPath(
			filePath, cosProvider, s.tempBucketName, s.tempRegion, "exports",
		)
		if err != nil {
			return "", err
		}
		return s.tempBucketURL + key, nil
	}
	return filePath, nil
}

// GetFileURL returns a presigned download URL for the file
func (s *cosFileService) GetFileURL(ctx context.Context, filePath string) (string, error) {
	client, objectName, err := s.resolveCosObject(filePath)
	if err != nil {
		return "", err
	}
	if err := utils.SafeObjectKey(objectName); err != nil {
		return "", fmt.Errorf("invalid file path: %w", err)
	}
	// Generate presigned URL (valid for 24 hours)
	presignedURL, err := client.Object.GetPresignedURL(ctx, http.MethodGet, objectName, client.GetCredential().SecretID, client.GetCredential().SecretKey, 24*time.Hour, nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %w", err)
	}

	return presignedURL.String(), nil
}
