package file

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

// ossFileService implements the FileService interface for Aliyun OSS
// using the official Aliyun OSS SDK v2 (github.com/aliyun/alibabacloud-oss-go-sdk-v2).
type ossFileService struct {
	client          *oss.Client
	tempClient      *oss.Client
	endpoint        string
	region          string
	tempRegion      string
	pathPrefix      string
	bucketName      string
	tempBucketName  string
	bindingSource   string
	credentialScope string
	credentialRef   string
}

const (
	ossProvider = "oss"
	ossScheme   = ossProvider + "://"
)

var _ interfaces.PlannedFileService = (*ossFileService)(nil)

// newOSSClient creates an OSS client using the official Aliyun SDK v2.
func newOSSClient(endpoint, region, accessKey, secretKey string) (*oss.Client, error) {
	creds := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")

	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(creds).
		WithRegion(region).
		WithEndpoint(endpoint)

	return oss.NewClient(cfg), nil
}

// ossEnsureBucket checks if the bucket exists and creates it if missing.
func ossEnsureBucket(client *oss.Client, bucketName string) error {
	exists, err := client.IsBucketExist(context.Background(), bucketName)
	if err != nil {
		return fmt.Errorf("failed to check OSS bucket: %w", err)
	}
	if exists {
		return nil
	}

	_, err = client.PutBucket(context.Background(), &oss.PutBucketRequest{
		Bucket: oss.Ptr(bucketName),
	})
	if err != nil {
		var svcErr *oss.ServiceError
		if errors.As(err, &svcErr) && svcErr.StatusCode == http.StatusConflict {
			return nil
		}
		return fmt.Errorf("failed to create OSS bucket: %w", err)
	}
	return nil
}

// NewOssFileService creates an Aliyun OSS file service.
// It verifies that the bucket exists and creates it if missing.
func NewOssFileService(endpoint, region, accessKey, secretKey, bucketName, pathPrefix string) (interfaces.FileService, error) {
	return NewOssFileServiceWithTempBucket(endpoint, region, accessKey, secretKey, bucketName, pathPrefix, "", "")
}

// NewOssFileServiceWithTempBucket creates an Aliyun OSS file service with optional temp bucket.
func NewOssFileServiceWithTempBucket(endpoint, region, accessKey, secretKey, bucketName, pathPrefix, tempBucketName, tempRegion string) (interfaces.FileService, error) {
	svc, err := newOSSFileService(
		endpoint, region, accessKey, secretKey, bucketName, pathPrefix, tempBucketName, tempRegion,
	)
	if err != nil {
		return nil, err
	}
	if err := ossEnsureBucket(svc.client, bucketName); err != nil {
		return nil, err
	}
	if svc.tempClient != nil {
		if err := ossEnsureBucket(svc.tempClient, tempBucketName); err != nil {
			return nil, err
		}
	}
	return svc, nil
}

// newOSSFileService constructs clients without probing or provisioning any
// bucket. Historical binding resolution must remain read-only.
func newOSSFileService(
	endpoint, region, accessKey, secretKey, bucketName, pathPrefix, tempBucketName, tempRegion string,
) (*ossFileService, error) {
	client, err := newOSSClient(endpoint, region, accessKey, secretKey)
	if err != nil {
		return nil, err
	}

	var tempClient *oss.Client
	if tempBucketName != "" {
		if tempRegion == "" {
			tempRegion = region
		}
		tempClient, err = newOSSClient(endpoint, tempRegion, accessKey, secretKey)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize OSS temp client: %w", err)
		}
	}

	// Normalize pathPrefix: ensure it ends with '/' if not empty
	if pathPrefix != "" && !strings.HasSuffix(pathPrefix, "/") {
		pathPrefix += "/"
	}

	credentialRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeDirect, storagebinding.ProviderOSS, "default",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to identify OSS credentials: %w", err)
	}
	return &ossFileService{
		client:          client,
		tempClient:      tempClient,
		endpoint:        endpoint,
		region:          region,
		tempRegion:      tempRegion,
		pathPrefix:      pathPrefix,
		bucketName:      bucketName,
		tempBucketName:  tempBucketName,
		bindingSource:   "direct",
		credentialScope: "direct",
		credentialRef:   credentialRef,
	}, nil
}

func (s *ossFileService) ReserveFilePath(
	tenantID uint64, knowledgeID string, fileName string,
) (string, error) {
	key, err := plannedfile.FileKey(s.pathPrefix, tenantID, knowledgeID, fileName)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath(ossProvider, s.bucketName, key)
}

func (s *ossFileService) ReserveBytesPath(
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
		return plannedfile.FormatBucketPath(ossProvider, s.tempBucketName, key)
	}
	key, err := plannedfile.BytesKey(s.pathPrefix, tenantID, fileName, "exports")
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath(ossProvider, s.bucketName, key)
}

func (s *ossFileService) ReserveCopyPath(
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	srcKey, err := plannedfile.ParseBucketPath(srcPath, ossProvider, s.bucketName, "")
	if err != nil {
		return "", fmt.Errorf("oss copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}
	return s.ReserveFilePath(tenantID, knowledgeID, "copy"+path.Ext(srcKey))
}

func (s *ossFileService) plannedMainKey(filePath string) (string, error) {
	key, err := plannedfile.ParseBucketPath(filePath, ossProvider, s.bucketName, s.pathPrefix)
	if err != nil {
		return "", fmt.Errorf("planned OSS destination is not bound to this service: %w", err)
	}
	return key, nil
}

func (s *ossFileService) plannedBytesTarget(filePath string) (*oss.Client, string, string, error) {
	if key, err := s.plannedMainKey(filePath); err == nil {
		return s.client, s.bucketName, key, nil
	}
	if s.tempClient != nil {
		key, err := plannedfile.ParseBucketPath(filePath, ossProvider, s.tempBucketName, "exports")
		if err == nil {
			return s.tempClient, s.tempBucketName, key, nil
		}
	}
	return nil, "", "", fmt.Errorf("planned OSS bytes destination is not bound to this service")
}

func (s *ossFileService) CommitFileAtPath(
	ctx context.Context, file *multipart.FileHeader, filePath string,
) error {
	if file == nil {
		return fmt.Errorf("planned OSS commit: file is nil")
	}
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("planned OSS commit: open upload: %w", err)
	}
	defer src.Close()
	ext := filepath.Ext(file.Filename)
	contentType := file.Header.Get("Content-Type")
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(ext)
	}
	const multipartThreshold = 10 * 1024 * 1024
	if file.Size > multipartThreshold {
		uploader := s.client.NewUploader(func(uo *oss.UploaderOptions) {
			uo.PartSize = 10 * 1024 * 1024
			uo.ParallelNum = 3
		})
		if _, err := uploader.UploadFrom(ctx, &oss.PutObjectRequest{
			Bucket: oss.Ptr(s.bucketName), Key: oss.Ptr(key), ContentType: oss.Ptr(contentType),
		}, src); err != nil {
			return fmt.Errorf("planned OSS commit: multipart upload: %w", err)
		}
		return nil
	}
	if _, err := s.client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket: oss.Ptr(s.bucketName), Key: oss.Ptr(key), Body: src, ContentType: oss.Ptr(contentType),
	}); err != nil {
		return fmt.Errorf("planned OSS commit: upload: %w", err)
	}
	return nil
}

func (s *ossFileService) commitReaderAtPath(
	ctx context.Context, reader io.ReadSeeker, size int64, contentType, filePath string,
) error {
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(path.Ext(key))
	}
	const multipartThreshold = 10 * 1024 * 1024
	if size > multipartThreshold {
		uploader := s.client.NewUploader(func(options *oss.UploaderOptions) {
			options.PartSize = multipartThreshold
			options.ParallelNum = 3
		})
		if _, err := uploader.UploadFrom(ctx, &oss.PutObjectRequest{
			Bucket: oss.Ptr(s.bucketName), Key: oss.Ptr(key), ContentType: oss.Ptr(contentType),
		}, reader); err != nil {
			return fmt.Errorf("planned OSS stream multipart commit: %w", err)
		}
		return nil
	}
	if _, err := s.client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket: oss.Ptr(s.bucketName), Key: oss.Ptr(key), Body: reader, ContentType: oss.Ptr(contentType),
	}); err != nil {
		return fmt.Errorf("planned OSS stream commit: %w", err)
	}
	return nil
}

func (s *ossFileService) CommitBytesAtPath(ctx context.Context, data []byte, filePath string) error {
	client, bucket, key, err := s.plannedBytesTarget(filePath)
	if err != nil {
		return err
	}
	if _, err := client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket: oss.Ptr(bucket), Key: oss.Ptr(key), Body: bytes.NewReader(data),
		ContentType: oss.Ptr(utils.GetContentTypeByExt(path.Ext(key))),
	}); err != nil {
		return fmt.Errorf("planned OSS bytes commit: upload: %w", err)
	}
	return nil
}

func (s *ossFileService) CommitCopyAtPath(ctx context.Context, srcPath string, dstPath string) error {
	srcKey, err := plannedfile.ParseBucketPath(srcPath, ossProvider, s.bucketName, "")
	if err != nil {
		return fmt.Errorf("planned OSS copy source: %w", err)
	}
	dstKey, err := s.plannedMainKey(dstPath)
	if err != nil {
		return err
	}
	if _, err := s.client.CopyObject(ctx, &oss.CopyObjectRequest{
		Bucket: oss.Ptr(s.bucketName), Key: oss.Ptr(dstKey),
		SourceBucket: oss.Ptr(s.bucketName), SourceKey: oss.Ptr(srcKey),
	}); err != nil {
		return fmt.Errorf("planned OSS copy commit: %w", err)
	}
	return nil
}

// CheckOssConnectivity tests OSS connectivity using the provided credentials.
func CheckOssConnectivity(ctx context.Context, endpoint, region, accessKey, secretKey, bucketName string) error {
	client, err := newOSSClient(endpoint, region, accessKey, secretKey)
	if err != nil {
		return err
	}

	exists, err := client.IsBucketExist(ctx, bucketName)
	if err != nil {
		return fmt.Errorf("failed to check OSS bucket: %w", err)
	}
	if !exists {
		return fmt.Errorf("bucket %q does not exist or is not accessible", bucketName)
	}
	return nil
}

// parseOssFilePath extracts bucket and object key from: oss://{bucket}/{objectKey}
func parseOssFilePath(filePath string) (bucketName string, objectKey string, err error) {
	if !strings.HasPrefix(filePath, ossScheme) {
		return "", "", fmt.Errorf("invalid OSS file path: %s", filePath)
	}

	rest := strings.TrimPrefix(filePath, ossScheme)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid OSS file path: %s", filePath)
	}
	return parts[0], parts[1], nil
}

// CheckConnectivity verifies OSS is reachable and the main bucket exists.
func (s *ossFileService) CheckConnectivity(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	exists, err := s.client.IsBucketExist(checkCtx, s.bucketName)
	if err != nil {
		return fmt.Errorf("failed to check OSS bucket: %w", err)
	}
	if !exists {
		return fmt.Errorf("bucket %q does not exist", s.bucketName)
	}
	return nil
}

// SaveFile saves a file to OSS using the Uploader manager for large files.
func (s *ossFileService) SaveFile(ctx context.Context,
	file *multipart.FileHeader, tenantID uint64, knowledgeID string,
) (string, error) {
	if file == nil {
		return "", fmt.Errorf("failed to save file to OSS: file is nil")
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

// SaveBytes saves bytes data to OSS.
// If temp is true and temp bucket is configured, saves to temp bucket.
// Otherwise saves to main bucket.
func (s *ossFileService) SaveBytes(ctx context.Context, data []byte, tenantID uint64, fileName string, temp bool) (string, error) {
	filePath, err := s.ReserveBytesPath(tenantID, fileName, temp)
	if err != nil {
		return "", err
	}
	if err := s.CommitBytesAtPath(ctx, data, filePath); err != nil {
		return "", err
	}
	return filePath, nil
}

// CopyFile copies an existing OSS object to a new knowledge-owned object using a
// server-side CopyObject (no data leaves OSS). The destination uses the same
// layout as SaveFile. Returns ErrCrossBackendCopy when srcPath is not an oss:// path.
func (s *ossFileService) CopyFile(ctx context.Context,
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	newPath, err := s.ReserveCopyPath(srcPath, tenantID, knowledgeID)
	if err != nil {
		return "", err
	}
	if err := s.CommitCopyAtPath(ctx, srcPath, newPath); err != nil {
		return "", err
	}
	logger.Infof(ctx, "Copied OSS object %s to %s", srcPath, newPath)
	return newPath, nil
}

func (s *ossFileService) clientForBoundBucket(bucket string) (*oss.Client, error) {
	if bucket == s.bucketName {
		return s.client, nil
	}
	if s.tempClient != nil && bucket == s.tempBucketName {
		return s.tempClient, nil
	}
	return nil, fmt.Errorf("OSS bucket %q is not bound to this service", bucket)
}

// GetFile retrieves a file from OSS by its path.
func (s *ossFileService) GetFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	bucketName, objectName, err := parseOssFilePath(filePath)
	if err != nil {
		return nil, err
	}
	if err := utils.SafeObjectKey(objectName); err != nil {
		return nil, fmt.Errorf("invalid file path: %w", err)
	}

	client, err := s.clientForBoundBucket(bucketName)
	if err != nil {
		return nil, err
	}

	resp, err := client.GetObject(ctx, &oss.GetObjectRequest{
		Bucket: oss.Ptr(bucketName),
		Key:    oss.Ptr(objectName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get file from OSS: %w", err)
	}

	return resp.Body, nil
}

// DeleteFile removes a file from OSS.
func (s *ossFileService) DeleteFile(ctx context.Context, filePath string) error {
	bucketName, objectName, err := parseOssFilePath(filePath)
	if err != nil {
		return err
	}
	if err := utils.SafeObjectKey(objectName); err != nil {
		return fmt.Errorf("invalid file path: %w", err)
	}

	client, err := s.clientForBoundBucket(bucketName)
	if err != nil {
		return err
	}

	_, err = client.DeleteObject(ctx, &oss.DeleteObjectRequest{
		Bucket: oss.Ptr(bucketName),
		Key:    oss.Ptr(objectName),
	})
	if err != nil {
		return fmt.Errorf("failed to delete file from OSS: %w", err)
	}

	return nil
}

// GetFileURL returns a presigned download URL for the file.
func (s *ossFileService) GetFileURL(ctx context.Context, filePath string) (string, error) {
	bucketName, objectName, err := parseOssFilePath(filePath)
	if err != nil {
		return "", err
	}
	if err := utils.SafeObjectKey(objectName); err != nil {
		return "", fmt.Errorf("invalid file path: %w", err)
	}

	client, err := s.clientForBoundBucket(bucketName)
	if err != nil {
		return "", err
	}

	// Generate presigned URL (valid for 24 hours)
	result, err := client.Presign(ctx, &oss.GetObjectRequest{
		Bucket: oss.Ptr(bucketName),
		Key:    oss.Ptr(objectName),
	}, oss.PresignExpires(24*time.Hour))
	if err != nil {
		return "", fmt.Errorf("failed to generate OSS presigned URL: %w", err)
	}

	return result.URL, nil
}
