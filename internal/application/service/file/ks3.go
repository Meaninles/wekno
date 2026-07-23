package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	ks3aws "github.com/ks3sdklib/aws-sdk-go/aws"
	"github.com/ks3sdklib/aws-sdk-go/aws/credentials"
	ks3s3 "github.com/ks3sdklib/aws-sdk-go/service/s3"
)

const (
	ks3Provider = "ks3"
	ks3Scheme   = ks3Provider + "://"
)

// ks3FileService implements FileService for Kingsoft Cloud KS3.
// KS3 uses V2 signing by default and virtual-hosted style addressing,
// so it cannot be handled by the generic S3 provider without workarounds.
type ks3FileService struct {
	client          *ks3s3.S3
	endpoint        string
	region          string
	bucketName      string
	pathPrefix      string
	bindingSource   string
	credentialScope string
	credentialRef   string
}

var _ interfaces.PlannedFileService = (*ks3FileService)(nil)

// NewKS3FileService creates a KS3 file service and ensures the bucket exists.
func NewKS3FileService(endpoint, region, accessKey, secretKey, bucketName, pathPrefix string) (interfaces.FileService, error) {
	svc, err := newKS3FileService(endpoint, region, accessKey, secretKey, bucketName, pathPrefix)
	if err != nil {
		return nil, err
	}
	if err := ensureKS3Bucket(svc.client, bucketName); err != nil {
		return nil, err
	}
	return svc, nil
}

// newKS3FileService constructs a client without probing or provisioning the
// bucket. Historical binding resolution must remain read-only.
func newKS3FileService(
	endpoint, region, accessKey, secretKey, bucketName, pathPrefix string,
) (*ks3FileService, error) {
	client, err := newKS3Client(endpoint, region, accessKey, secretKey)
	if err != nil {
		return nil, err
	}

	pathPrefix = strings.Trim(pathPrefix, "/")
	credentialRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeDirect, storagebinding.ProviderKS3, "default",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to identify KS3 credentials: %w", err)
	}

	svc := &ks3FileService{
		client:          client,
		endpoint:        endpoint,
		region:          region,
		bucketName:      bucketName,
		pathPrefix:      pathPrefix,
		bindingSource:   "direct",
		credentialScope: "direct",
		credentialRef:   credentialRef,
	}

	return svc, nil
}

func (s *ks3FileService) ReserveFilePath(
	tenantID uint64, knowledgeID string, fileName string,
) (string, error) {
	key, err := plannedfile.FileKey(s.pathPrefix, tenantID, knowledgeID, fileName)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath(ks3Provider, s.bucketName, key)
}

func (s *ks3FileService) ReserveBytesPath(
	tenantID uint64, fileName string, _ bool,
) (string, error) {
	key, err := plannedfile.BytesKey(s.pathPrefix, tenantID, fileName, "exports")
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath(ks3Provider, s.bucketName, key)
}

func (s *ks3FileService) ReserveCopyPath(
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	srcKey, err := plannedfile.ParseBucketPath(srcPath, ks3Provider, s.bucketName, "")
	if err != nil {
		return "", fmt.Errorf("ks3 copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}
	return s.ReserveFilePath(tenantID, knowledgeID, "copy"+path.Ext(srcKey))
}

func (s *ks3FileService) plannedMainKey(filePath string) (string, error) {
	key, err := plannedfile.ParseBucketPath(filePath, ks3Provider, s.bucketName, s.pathPrefix)
	if err != nil {
		return "", fmt.Errorf("planned KS3 destination is not bound to this service: %w", err)
	}
	return key, nil
}

func (s *ks3FileService) CommitFileAtPath(
	ctx context.Context, file *multipart.FileHeader, filePath string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if file == nil {
		return fmt.Errorf("planned KS3 commit: file is nil")
	}
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("planned KS3 commit: open upload: %w", err)
	}
	defer src.Close()
	ext := filepath.Ext(file.Filename)
	contentType := file.Header.Get("Content-Type")
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(ext)
	}
	_, err = s.client.PutObject(&ks3s3.PutObjectInput{
		Bucket: ks3aws.String(s.bucketName), Key: ks3aws.String(key), Body: src,
		ContentType: ks3aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("planned KS3 commit: upload: %w", err)
	}
	return nil
}

func (s *ks3FileService) commitReaderAtPath(
	ctx context.Context, reader io.ReadSeeker, _ int64, contentType, filePath string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(path.Ext(key))
	}
	if _, err := s.client.PutObject(&ks3s3.PutObjectInput{
		Bucket: ks3aws.String(s.bucketName), Key: ks3aws.String(key), Body: reader,
		ContentType: ks3aws.String(contentType),
	}); err != nil {
		return fmt.Errorf("planned KS3 stream commit: %w", err)
	}
	return nil
}

func (s *ks3FileService) CommitBytesAtPath(ctx context.Context, data []byte, filePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(&ks3s3.PutObjectInput{
		Bucket: ks3aws.String(s.bucketName), Key: ks3aws.String(key), Body: bytes.NewReader(data),
		ContentType: ks3aws.String(utils.GetContentTypeByExt(path.Ext(key))),
	})
	if err != nil {
		return fmt.Errorf("planned KS3 bytes commit: upload: %w", err)
	}
	return nil
}

func (s *ks3FileService) CommitCopyAtPath(ctx context.Context, srcPath string, dstPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	srcKey, err := plannedfile.ParseBucketPath(srcPath, ks3Provider, s.bucketName, "")
	if err != nil {
		return fmt.Errorf("planned KS3 copy source: %w", err)
	}
	dstKey, err := s.plannedMainKey(dstPath)
	if err != nil {
		return err
	}
	_, err = s.client.CopyObject(&ks3s3.CopyObjectInput{
		Bucket: ks3aws.String(s.bucketName), Key: ks3aws.String(dstKey),
		SourceBucket: ks3aws.String(s.bucketName), SourceKey: ks3aws.String(srcKey),
	})
	if err != nil {
		return fmt.Errorf("planned KS3 copy commit: %w", err)
	}
	return nil
}

func newKS3Client(endpoint, region, accessKey, secretKey string) (*ks3s3.S3, error) {
	creds := credentials.NewStaticCredentials(accessKey, secretKey, "")
	client := ks3s3.New(&ks3aws.Config{
		Credentials:      creds,
		Region:           region,
		Endpoint:         endpoint,
		DisableSSL:       false,
		S3ForcePathStyle: false, // KS3 uses virtual-hosted style
		SignerVersion:    "V2",  // KS3 recommends V2 signing
		MaxRetries:       3,
	})
	return client, nil
}

func ensureKS3Bucket(client *ks3s3.S3, bucketName string) error {
	_, err := client.HeadBucket(&ks3s3.HeadBucketInput{
		Bucket: ks3aws.String(bucketName),
	})
	if err == nil {
		return nil
	}
	// Bucket doesn't exist, try to create it
	_, createErr := client.CreateBucket(&ks3s3.CreateBucketInput{
		Bucket: ks3aws.String(bucketName),
	})
	if createErr != nil {
		return fmt.Errorf("failed to create KS3 bucket %q: %w", bucketName, createErr)
	}
	return nil
}

// CheckKS3Connectivity tests KS3 connectivity using the provided credentials.
func CheckKS3Connectivity(ctx context.Context, endpoint, region, accessKey, secretKey, bucketName string) error {
	client, err := newKS3Client(endpoint, region, accessKey, secretKey)
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		_, err := client.HeadBucket(&ks3s3.HeadBucketInput{
			Bucket: ks3aws.String(bucketName),
		})
		done <- err
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func joinKS3Key(parts ...string) string {
	filtered := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(p, "/")
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	return strings.Join(filtered, "/")
}

func parseKS3FilePath(filePath string) (bucket, objectKey string, err error) {
	if !strings.HasPrefix(filePath, ks3Scheme) {
		return "", "", fmt.Errorf("invalid KS3 file path: %s", filePath)
	}
	rest := strings.TrimPrefix(filePath, ks3Scheme)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid KS3 file path: %s", filePath)
	}
	return parts[0], parts[1], nil
}

func (s *ks3FileService) SaveFile(ctx context.Context, file *multipart.FileHeader, tenantID uint64, knowledgeID string) (string, error) {
	if file == nil {
		return "", fmt.Errorf("failed to save file to KS3: file is nil")
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

func (s *ks3FileService) SaveBytes(ctx context.Context, data []byte, tenantID uint64, fileName string, temp bool) (string, error) {
	filePath, err := s.ReserveBytesPath(tenantID, fileName, temp)
	if err != nil {
		return "", err
	}
	if err := s.CommitBytesAtPath(ctx, data, filePath); err != nil {
		return "", err
	}
	return filePath, nil
}

// CopyFile copies an existing KS3 object to a new knowledge-owned object using a
// server-side CopyObject (no data leaves KS3). The destination uses the same
// layout as SaveFile. Returns ErrCrossBackendCopy when srcPath is not a ks3:// path.
func (s *ks3FileService) CopyFile(ctx context.Context,
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	newPath, err := s.ReserveCopyPath(srcPath, tenantID, knowledgeID)
	if err != nil {
		return "", err
	}
	if err := s.CommitCopyAtPath(ctx, srcPath, newPath); err != nil {
		return "", err
	}
	logger.Infof(ctx, "Copied KS3 object %s to %s", srcPath, newPath)
	return newPath, nil
}

func (s *ks3FileService) GetFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	bucket, objectKey, err := parseKS3FilePath(filePath)
	if err != nil {
		return nil, err
	}
	if bucket != s.bucketName {
		return nil, fmt.Errorf("KS3 bucket %q is not bound to this service", bucket)
	}
	if err := utils.SafeObjectKey(objectKey); err != nil {
		return nil, fmt.Errorf("invalid file path: %w", err)
	}

	resp, err := s.client.GetObject(&ks3s3.GetObjectInput{
		Bucket: ks3aws.String(s.bucketName),
		Key:    ks3aws.String(objectKey),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get file from KS3: %w", err)
	}

	return resp.Body, nil
}

func (s *ks3FileService) DeleteFile(ctx context.Context, filePath string) error {
	bucket, objectKey, err := parseKS3FilePath(filePath)
	if err != nil {
		return err
	}
	if bucket != s.bucketName {
		return fmt.Errorf("KS3 bucket %q is not bound to this service", bucket)
	}
	if err := utils.SafeObjectKey(objectKey); err != nil {
		return fmt.Errorf("invalid file path: %w", err)
	}

	_, err = s.client.DeleteObject(&ks3s3.DeleteObjectInput{
		Bucket: ks3aws.String(s.bucketName),
		Key:    ks3aws.String(objectKey),
	})
	if err != nil {
		return fmt.Errorf("failed to delete file from KS3: %w", err)
	}
	return nil
}

func (s *ks3FileService) CheckConnectivity(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		_, err := s.client.HeadBucket(&ks3s3.HeadBucketInput{
			Bucket: ks3aws.String(s.bucketName),
		})
		done <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func (s *ks3FileService) GetFileURL(ctx context.Context, filePath string) (string, error) {
	bucket, objectKey, err := parseKS3FilePath(filePath)
	if err != nil {
		return "", err
	}
	if bucket != s.bucketName {
		return "", fmt.Errorf("KS3 bucket %q is not bound to this service", bucket)
	}
	if err := utils.SafeObjectKey(objectKey); err != nil {
		return "", fmt.Errorf("invalid file path: %w", err)
	}

	url, err := s.client.GeneratePresignedUrl(&ks3s3.GeneratePresignedUrlInput{
		Bucket:     ks3aws.String(s.bucketName),
		Key:        ks3aws.String(objectKey),
		HTTPMethod: ks3s3.HTTPMethod("GET"),
		Expires:    int64((24 * time.Hour).Seconds()),
	})
	if err != nil {
		return "", fmt.Errorf("failed to generate KS3 presigned URL: %w", err)
	}

	return url, nil
}
