package file

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
)

// s3FileService AWS S3 file service implementation
type s3FileService struct {
	client          *s3.Client
	endpoint        string
	region          string
	bucketName      string
	pathPrefix      string
	usePathStyle    bool
	bindingSource   string
	credentialScope string
	credentialRef   string
}

var _ interfaces.PlannedFileService = (*s3FileService)(nil)

// newS3Client creates a bare s3FileService with just the SDK client initialised.
func newS3Client(
	endpoint, accessKey, secretKey, bucketName, region, pathPrefix string,
	forcePathStyle ...bool,
) (*s3FileService, error) {
	var cfg aws.Config
	var err error

	// Configure AWS SDK
	cfg, err = config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)

	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create S3 client with custom endpoint if provided.
	// For S3-compatible services (non-AWS), use path-style addressing
	// (endpoint/bucket/key) instead of virtual-hosted style (bucket.endpoint/key).
	var client *s3.Client
	usePathStyle := false
	if endpoint != "" {
		usePathStyle = !strings.Contains(endpoint, "amazonaws.com")
		if len(forcePathStyle) > 0 {
			usePathStyle = forcePathStyle[0]
		}
		client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = usePathStyle
		})
	} else {
		// Standard AWS S3
		client = s3.NewFromConfig(cfg)
	}

	// Normalize pathPrefix: ensure it ends with '/' if not empty
	if pathPrefix != "" && !strings.HasSuffix(pathPrefix, "/") {
		pathPrefix += "/"
	}

	credentialRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeDirect, storagebinding.ProviderS3, "default",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to identify S3 credentials: %w", err)
	}
	return &s3FileService{
		client:          client,
		endpoint:        endpoint,
		region:          region,
		bucketName:      bucketName,
		pathPrefix:      pathPrefix,
		usePathStyle:    usePathStyle,
		bindingSource:   "direct",
		credentialScope: "direct",
		credentialRef:   credentialRef,
	}, nil
}

// NewS3FileService creates an AWS S3 file service.
// It verifies that the bucket exists and creates it if missing.
func NewS3FileService(endpoint,
	accessKey, secretKey, bucketName, region, pathPrefix string,
) (interfaces.FileService, error) {
	return newProvisionedS3FileService(
		endpoint, accessKey, secretKey, bucketName, region, pathPrefix,
	)
}

// NewS3FileServiceWithPathStyle creates an S3 service with an explicit
// addressing mode. Tenant configuration uses this when ForcePathStyle is set.
func NewS3FileServiceWithPathStyle(
	endpoint, accessKey, secretKey, bucketName, region, pathPrefix string,
	usePathStyle bool,
) (interfaces.FileService, error) {
	return newProvisionedS3FileService(
		endpoint, accessKey, secretKey, bucketName, region, pathPrefix, usePathStyle,
	)
}

func newProvisionedS3FileService(
	endpoint, accessKey, secretKey, bucketName, region, pathPrefix string,
	forcePathStyle ...bool,
) (interfaces.FileService, error) {
	svc, err := newS3Client(
		endpoint, accessKey, secretKey, bucketName, region, pathPrefix, forcePathStyle...,
	)
	if err != nil {
		return nil, err
	}

	// Check if bucket exists
	exists, err := svc.bucketExists(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to check bucket: %w", err)
	}

	if !exists {
		if err = svc.createBucket(context.Background()); err != nil {
			return nil, fmt.Errorf("failed to create bucket: %w", err)
		}
	}

	return svc, nil
}

// bucketExists checks if the bucket exists
func (s *s3FileService) bucketExists(ctx context.Context) (bool, error) {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucketName),
	})
	if err != nil {
		// Check if the error is a NotFound error
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// createBucket creates a new bucket
func (s *s3FileService) createBucket(ctx context.Context) error {
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(s.bucketName),
	})
	return err
}

// CheckConnectivity verifies S3 is reachable and, if a bucket is configured,
// that the bucket exists. This is a read-only probe — it never creates a bucket.
func (s *s3FileService) CheckConnectivity(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if s.bucketName != "" {
		exists, err := s.bucketExists(checkCtx)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("bucket %q does not exist", s.bucketName)
		}
		return nil
	}

	// List buckets to verify connectivity
	_, err := s.client.ListBuckets(checkCtx, &s3.ListBucketsInput{})
	return err
}

// CheckS3Connectivity tests S3 connectivity using the provided credentials.
// It creates a temporary service instance internally and delegates to CheckConnectivity.
func CheckS3Connectivity(ctx context.Context, endpoint, accessKey, secretKey, bucketName, region string) error {
	svc, err := newS3Client(endpoint, accessKey, secretKey, bucketName, region, "")
	if err != nil {
		return err
	}
	return svc.CheckConnectivity(ctx)
}

// parseS3FilePath extracts the object name from a provider scheme: s3://{bucket}/{objectKey}
func (s *s3FileService) parseS3FilePath(filePath string) (string, error) {
	// Provider scheme format: s3://{bucket}/{objectKey}
	const prefix = "s3://"
	if !strings.HasPrefix(filePath, prefix) {
		return "", fmt.Errorf("invalid S3 file path: %s", filePath)
	}
	rest := strings.TrimPrefix(filePath, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid S3 file path: %s", filePath)
	}
	if parts[0] != s.bucketName {
		return "", fmt.Errorf("bucket mismatch in path: got %s, want %s", parts[0], s.bucketName)
	}
	if err := utils.SafeObjectKey(parts[1]); err != nil {
		return "", fmt.Errorf("invalid file path: %w", err)
	}
	return parts[1], nil
}

func (s *s3FileService) ReserveFilePath(
	tenantID uint64, knowledgeID string, fileName string,
) (string, error) {
	key, err := plannedfile.FileKey(s.pathPrefix, tenantID, knowledgeID, fileName)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath("s3", s.bucketName, key)
}

func (s *s3FileService) ReserveBytesPath(
	tenantID uint64, fileName string, _ bool,
) (string, error) {
	key, err := plannedfile.BytesKey(s.pathPrefix, tenantID, fileName, "exports")
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath("s3", s.bucketName, key)
}

func (s *s3FileService) ReserveCopyPath(
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	srcKey, err := s.parsePlannedS3Path(srcPath, false)
	if err != nil {
		return "", fmt.Errorf("s3 copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}
	return s.ReserveFilePath(tenantID, knowledgeID, "copy"+path.Ext(srcKey))
}

func (s *s3FileService) CommitFileAtPath(
	ctx context.Context, file *multipart.FileHeader, filePath string,
) error {
	if file == nil {
		return fmt.Errorf("planned S3 commit: file is nil")
	}
	key, err := s.parsePlannedS3Path(filePath, true)
	if err != nil {
		return err
	}
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("planned S3 commit: open upload: %w", err)
	}
	defer src.Close()
	contentType := file.Header.Get("Content-Type")
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(path.Ext(key))
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName), Key: aws.String(key), Body: src,
		ContentLength: aws.Int64(file.Size), ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("planned S3 commit: put exact object: %w", err)
	}
	return nil
}

func (s *s3FileService) commitReaderAtPath(
	ctx context.Context, reader io.ReadSeeker, size int64, contentType, filePath string,
) error {
	key, err := s.parsePlannedS3Path(filePath, true)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(path.Ext(key))
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName), Key: aws.String(key), Body: reader,
		ContentLength: aws.Int64(size), ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("planned S3 stream commit: %w", err)
	}
	return nil
}

func (s *s3FileService) CommitBytesAtPath(ctx context.Context, data []byte, filePath string) error {
	key, err := s.parsePlannedS3Path(filePath, true)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName), Key: aws.String(key), Body: bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(utils.GetContentTypeByExt(path.Ext(key))),
	})
	if err != nil {
		return fmt.Errorf("planned S3 commit: put exact bytes object: %w", err)
	}
	return nil
}

func (s *s3FileService) CommitCopyAtPath(ctx context.Context, srcPath string, dstPath string) error {
	srcKey, err := s.parsePlannedS3Path(srcPath, false)
	if err != nil {
		return fmt.Errorf("planned S3 copy source: %w", err)
	}
	dstKey, err := s.parsePlannedS3Path(dstPath, true)
	if err != nil {
		return fmt.Errorf("planned S3 copy destination: %w", err)
	}
	_, err = s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(s.bucketName), CopySource: aws.String(s.bucketName + "/" + srcKey),
		Key: aws.String(dstKey),
	})
	if err != nil {
		return fmt.Errorf("planned S3 commit: copy exact object: %w", err)
	}
	return nil
}

func (s *s3FileService) parsePlannedS3Path(filePath string, requirePrefix bool) (string, error) {
	prefix := ""
	if requirePrefix {
		prefix = s.pathPrefix
	}
	return plannedfile.ParseBucketPath(filePath, "s3", s.bucketName, prefix)
}

// SaveFile saves a file to S3
func (s *s3FileService) SaveFile(ctx context.Context,
	file *multipart.FileHeader, tenantID uint64, knowledgeID string,
) (string, error) {
	// Generate object name
	ext := filepath.Ext(file.Filename)
	objectName := fmt.Sprintf("%s%d/%s/%s%s", s.pathPrefix, tenantID, knowledgeID, uuid.New().String(), ext)

	// Open file
	src, err := file.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer src.Close()

	// Determine content type
	contentType := file.Header.Get("Content-Type")
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(ext)
	}

	// Upload file to S3
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucketName),
		Key:           aws.String(objectName),
		Body:          src,
		ContentLength: aws.Int64(file.Size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload file to S3: %w", err)
	}

	return fmt.Sprintf("s3://%s/%s", s.bucketName, objectName), nil
}

// GetFile gets a file from S3
func (s *s3FileService) GetFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	objectName, err := s.parseS3FilePath(filePath)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(objectName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get file from S3: %w", err)
	}

	return resp.Body, nil
}

// DeleteFile deletes a file
func (s *s3FileService) DeleteFile(ctx context.Context, filePath string) error {
	objectName, err := s.parseS3FilePath(filePath)
	if err != nil {
		return err
	}

	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(objectName),
	})
	if err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return nil
}

// CopyFile copies an existing S3 object to a new knowledge-owned object using a
// server-side CopyObject (no data leaves S3). The destination uses the same
// layout as SaveFile. Returns ErrCrossBackendCopy when srcPath is not an s3:// path.
func (s *s3FileService) CopyFile(ctx context.Context,
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	srcKey, err := s.parseS3FilePath(srcPath)
	if err != nil {
		return "", fmt.Errorf("s3 copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}

	ext := filepath.Ext(srcPath)
	destKey := fmt.Sprintf("%s%d/%s/%s%s", s.pathPrefix, tenantID, knowledgeID, uuid.New().String(), ext)

	// CopySource is "bucket/key"; the '/' separators must NOT be percent-encoded
	// (url.PathEscape would turn them into %2F and break the bucket/key split).
	// srcKey is already validated by parseS3FilePath -> SafeObjectKey.
	_, err = s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucketName),
		CopySource: aws.String(s.bucketName + "/" + srcKey),
		Key:        aws.String(destKey),
	})
	if err != nil {
		return "", fmt.Errorf("failed to copy file in S3: %w", err)
	}

	newPath := fmt.Sprintf("s3://%s/%s", s.bucketName, destKey)
	logger.Infof(ctx, "Copied S3 object %s to %s", srcPath, newPath)
	return newPath, nil
}

// SaveBytes saves bytes data to S3 and returns the file path
// temp parameter is ignored for S3 (no auto-expiration support in this implementation)
func (s *s3FileService) SaveBytes(ctx context.Context, data []byte, tenantID uint64, fileName string, temp bool) (string, error) {
	safeName, err := utils.SafeFileName(fileName)
	if err != nil {
		return "", fmt.Errorf("invalid file name: %w", err)
	}
	ext := filepath.Ext(safeName)
	objectName := fmt.Sprintf("%s%d/exports/%s%s", s.pathPrefix, tenantID, uuid.New().String(), ext)

	// Upload bytes to S3
	reader := bytes.NewReader(data)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucketName),
		Key:           aws.String(objectName),
		Body:          reader,
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(utils.GetContentTypeByExt(ext)),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload bytes to S3: %w", err)
	}

	return fmt.Sprintf("s3://%s/%s", s.bucketName, objectName), nil
}

// GetFileURL returns a presigned download URL for the file
func (s *s3FileService) GetFileURL(ctx context.Context, filePath string) (string, error) {
	objectName, err := s.parseS3FilePath(filePath)
	if err != nil {
		return "", err
	}

	// Create presign client
	presignClient := s3.NewPresignClient(s.client)

	// Generate presigned URL
	presignedReq, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(objectName),
	}, s3.WithPresignExpires(24*time.Hour))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %w", err)
	}

	return presignedReq.URL, nil
}
