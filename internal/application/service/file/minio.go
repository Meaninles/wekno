package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
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
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// minioFileService MinIO file service implementation
type minioFileService struct {
	client          *minio.Client
	endpoint        string
	bucketName      string
	pathPrefix      string
	useSSL          bool
	bindingSource   string
	credentialScope string
	credentialRef   string
}

var _ interfaces.PlannedFileService = (*minioFileService)(nil)
var _ interfaces.PrivateObjectFileService = (*minioFileService)(nil)
var _ interfaces.StreamingPrivateObjectFileService = (*minioFileService)(nil)

// newMinioClient creates a bare minioFileService with just the SDK client initialised.
// Shared by NewMinioFileService (which also ensures the bucket exists) and
// CheckMinioConnectivity (read-only probe).
func newMinioClient(endpoint, accessKeyID, secretAccessKey, bucketName string, useSSL bool, pathPrefix string) (*minioFileService, error) {
	normalizedPrefix, err := normalizeMinioPathPrefix(pathPrefix)
	if err != nil {
		return nil, err
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize MinIO client: %w", err)
	}
	credentialRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeDirect, storagebinding.ProviderMinIO, "default",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to identify MinIO credentials: %w", err)
	}
	return &minioFileService{
		client:          client,
		endpoint:        endpoint,
		bucketName:      bucketName,
		pathPrefix:      normalizedPrefix,
		useSSL:          useSSL,
		bindingSource:   "direct",
		credentialScope: "direct",
		credentialRef:   credentialRef,
	}, nil
}

// NewMinioFileService creates a MinIO file service.
// It verifies that the bucket exists and creates it if missing.
func NewMinioFileService(endpoint,
	accessKeyID, secretAccessKey, bucketName string, useSSL bool,
) (interfaces.FileService, error) {
	return NewMinioFileServiceWithPathPrefix(endpoint, accessKeyID, secretAccessKey, bucketName, useSSL, "")
}

// NewMinioFileServiceWithPathPrefix creates a MinIO file service that writes
// new objects under a dedicated object-key prefix.
func NewMinioFileServiceWithPathPrefix(endpoint,
	accessKeyID, secretAccessKey, bucketName string, useSSL bool, pathPrefix string,
) (interfaces.FileService, error) {
	svc, err := newMinioClient(endpoint, accessKeyID, secretAccessKey, bucketName, useSSL, pathPrefix)
	if err != nil {
		return nil, err
	}

	exists, err := svc.client.BucketExists(context.Background(), bucketName)
	if err != nil {
		return nil, fmt.Errorf("failed to check bucket: %w", err)
	}
	if !exists {
		if err = svc.client.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("failed to create bucket: %w", err)
		}
	}

	return svc, nil
}

// CheckConnectivity verifies MinIO is reachable and, if a bucket is configured,
// that the bucket exists. This is a read-only probe — it never creates a bucket.
func (s *minioFileService) CheckConnectivity(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if s.bucketName != "" {
		exists, err := s.client.BucketExists(checkCtx, s.bucketName)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("bucket %q does not exist", s.bucketName)
		}
		return nil
	}
	_, err := s.client.ListBuckets(checkCtx)
	return err
}

// CheckMinioConnectivity tests MinIO connectivity using the provided credentials.
// It creates a temporary service instance internally and delegates to CheckConnectivity.
func CheckMinioConnectivity(ctx context.Context, endpoint, accessKeyID, secretAccessKey, bucketName string, useSSL bool) error {
	svc, err := newMinioClient(endpoint, accessKeyID, secretAccessKey, bucketName, useSSL, "")
	if err != nil {
		return err
	}
	return svc.CheckConnectivity(ctx)
}

func normalizeMinioPathPrefix(pathPrefix string) (string, error) {
	prefix := strings.ReplaceAll(strings.TrimSpace(pathPrefix), "\\", "/")
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	if err := utils.SafeObjectKey(prefix); err != nil {
		return "", fmt.Errorf("invalid MinIO path prefix: %w", err)
	}
	return prefix + "/", nil
}

func (s *minioFileService) prefixedObjectName(format string, args ...any) string {
	return s.pathPrefix + fmt.Sprintf(format, args...)
}

func (s *minioFileService) ReserveFilePath(
	tenantID uint64, knowledgeID string, fileName string,
) (string, error) {
	key, err := plannedfile.FileKey(s.pathPrefix, tenantID, knowledgeID, fileName)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath("minio", s.bucketName, key)
}

func (s *minioFileService) ReserveBytesPath(
	tenantID uint64, fileName string, temp bool,
) (string, error) {
	var key string
	var err error
	if temp {
		name, nameErr := plannedfile.NewObjectName(fileName)
		if nameErr != nil {
			return "", nameErr
		}
		key, err = plannedfile.BuildKey(s.pathPrefix, "temp", strconv.FormatUint(tenantID, 10), name)
	} else {
		key, err = plannedfile.BytesKey(s.pathPrefix, tenantID, fileName, "exports")
	}
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath("minio", s.bucketName, key)
}

func (s *minioFileService) ReserveCopyPath(
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	srcKey, err := s.parsePlannedMinioPath(srcPath, false)
	if err != nil {
		return "", fmt.Errorf("minio copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}
	return s.ReserveFilePath(tenantID, knowledgeID, "copy"+path.Ext(srcKey))
}

func (s *minioFileService) ReservePrivateObjectPath(segments ...string) (string, error) {
	key, err := plannedfile.BuildKey(s.pathPrefix, segments...)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath("minio", s.bucketName, key)
}

func (s *minioFileService) CommitPrivateObjectAtPath(
	ctx context.Context,
	data []byte,
	filePath string,
	contentType string,
	sha256 string,
) error {
	key, err := s.parsePlannedMinioPath(filePath, true)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = s.client.PutObject(
		ctx,
		s.bucketName,
		key,
		bytes.NewReader(data),
		int64(len(data)),
		minio.PutObjectOptions{
			ContentType: contentType,
			UserMetadata: map[string]string{
				"sha256": strings.ToLower(strings.TrimSpace(sha256)),
			},
		},
	)
	if err != nil {
		return fmt.Errorf("private MinIO object commit: %w", err)
	}
	return nil
}

func (s *minioFileService) CommitPrivateObjectStreamAtPath(
	ctx context.Context,
	reader io.ReadSeeker,
	size int64,
	filePath string,
	contentType string,
	sha256 string,
) error {
	key, err := s.parsePlannedMinioPath(filePath, true)
	if err != nil {
		return err
	}
	if reader == nil || size < 0 {
		return fmt.Errorf("private MinIO stream commit: invalid source")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = s.client.PutObject(
		ctx,
		s.bucketName,
		key,
		reader,
		size,
		minio.PutObjectOptions{
			ContentType: contentType,
			UserMetadata: map[string]string{
				"sha256": strings.ToLower(strings.TrimSpace(sha256)),
			},
		},
	)
	if err != nil {
		return fmt.Errorf("private MinIO stream commit: %w", err)
	}
	return nil
}

func (s *minioFileService) VerifyPrivateObject(
	ctx context.Context,
	filePath string,
	size int64,
	sha256 string,
) error {
	key, err := s.parsePlannedMinioPath(filePath, true)
	if err != nil {
		return err
	}
	info, err := s.client.StatObject(ctx, s.bucketName, key, minio.StatObjectOptions{})
	if err != nil {
		return fmt.Errorf("verify private MinIO object: %w", err)
	}
	if info.Size != size {
		return fmt.Errorf("verify private MinIO object: size mismatch: got %d want %d", info.Size, size)
	}
	wantSHA := strings.ToLower(strings.TrimSpace(sha256))
	gotSHA := strings.ToLower(strings.TrimSpace(info.UserMetadata["X-Amz-Meta-Sha256"]))
	if gotSHA == "" {
		gotSHA = strings.ToLower(strings.TrimSpace(info.UserMetadata["Sha256"]))
	}
	if wantSHA == "" || gotSHA != wantSHA {
		return fmt.Errorf("verify private MinIO object: sha256 metadata mismatch")
	}
	return nil
}

func (s *minioFileService) CommitFileAtPath(
	ctx context.Context, file *multipart.FileHeader, filePath string,
) error {
	if file == nil {
		return fmt.Errorf("planned MinIO commit: file is nil")
	}
	key, err := s.parsePlannedMinioPath(filePath, true)
	if err != nil {
		return err
	}
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("planned MinIO commit: open upload: %w", err)
	}
	defer src.Close()
	_, err = s.client.PutObject(ctx, s.bucketName, key, src, file.Size, minio.PutObjectOptions{
		ContentType: file.Header.Get("Content-Type"),
	})
	if err != nil {
		return fmt.Errorf("planned MinIO commit: put exact object: %w", err)
	}
	return nil
}

func (s *minioFileService) commitReaderAtPath(
	ctx context.Context, reader io.ReadSeeker, size int64, contentType, filePath string,
) error {
	key, err := s.parsePlannedMinioPath(filePath, true)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(path.Ext(key))
	}
	if _, err := s.client.PutObject(ctx, s.bucketName, key, reader, size, minio.PutObjectOptions{ContentType: contentType}); err != nil {
		return fmt.Errorf("planned MinIO stream commit: %w", err)
	}
	return nil
}

func (s *minioFileService) CommitBytesAtPath(ctx context.Context, data []byte, filePath string) error {
	key, err := s.parsePlannedMinioPath(filePath, true)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(
		ctx, s.bucketName, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: utils.GetContentTypeByExt(path.Ext(key))},
	)
	if err != nil {
		return fmt.Errorf("planned MinIO commit: put exact bytes object: %w", err)
	}
	return nil
}

func (s *minioFileService) CommitCopyAtPath(ctx context.Context, srcPath string, dstPath string) error {
	srcKey, err := s.parsePlannedMinioPath(srcPath, false)
	if err != nil {
		return fmt.Errorf("planned MinIO copy source: %w", err)
	}
	dstKey, err := s.parsePlannedMinioPath(dstPath, true)
	if err != nil {
		return fmt.Errorf("planned MinIO copy destination: %w", err)
	}
	_, err = s.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.bucketName, Object: dstKey},
		minio.CopySrcOptions{Bucket: s.bucketName, Object: srcKey},
	)
	if err != nil {
		return fmt.Errorf("planned MinIO commit: copy exact object: %w", err)
	}
	return nil
}

func (s *minioFileService) parsePlannedMinioPath(filePath string, requirePrefix bool) (string, error) {
	prefix := ""
	if requirePrefix {
		prefix = s.pathPrefix
	}
	return plannedfile.ParseBucketPath(filePath, "minio", s.bucketName, prefix)
}

// parseMinioFilePath extracts the object name from a provider scheme: minio://{bucket}/{objectKey}
func (s *minioFileService) parseMinioFilePath(filePath string) (string, error) {
	// Provider scheme format: minio://{bucket}/{objectKey}
	const prefix = "minio://"
	if !strings.HasPrefix(filePath, prefix) {
		return "", fmt.Errorf("invalid MinIO file path: %s", filePath)
	}
	rest := strings.TrimPrefix(filePath, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid MinIO file path: %s", filePath)
	}
	if parts[0] != s.bucketName {
		return "", fmt.Errorf("bucket mismatch in path: got %s, want %s", parts[0], s.bucketName)
	}
	if err := utils.SafeObjectKey(parts[1]); err != nil {
		return "", fmt.Errorf("invalid file path: %w", err)
	}
	return parts[1], nil
}

// SaveFile saves a file to MinIO
func (s *minioFileService) SaveFile(ctx context.Context,
	file *multipart.FileHeader, tenantID uint64, knowledgeID string,
) (string, error) {
	// Generate object name
	ext := filepath.Ext(file.Filename)
	objectName := s.prefixedObjectName("%d/%s/%s%s", tenantID, knowledgeID, uuid.New().String(), ext)

	// Open file
	src, err := file.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer src.Close()

	// Upload file to MinIO
	_, err = s.client.PutObject(ctx, s.bucketName, objectName, src, file.Size, minio.PutObjectOptions{
		ContentType: file.Header.Get("Content-Type"),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload file to MinIO: %w", err)
	}

	return fmt.Sprintf("minio://%s/%s", s.bucketName, objectName), nil
}

// GetFile gets a file from MinIO
func (s *minioFileService) GetFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	objectName, err := s.parseMinioFilePath(filePath)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get file from MinIO: %w", err)
	}
	return obj, nil
}

// DeleteFile deletes a file
func (s *minioFileService) DeleteFile(ctx context.Context, filePath string) error {
	objectName, err := s.parseMinioFilePath(filePath)
	if err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucketName, objectName, minio.RemoveObjectOptions{
		GovernanceBypass: true,
	}); err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}
	return nil
}

// CopyFile copies an existing MinIO object to a new knowledge-owned object using a
// server-side CopyObject (no data leaves MinIO). The destination uses the same
// layout as SaveFile. Returns ErrCrossBackendCopy when srcPath is not a minio:// path.
func (s *minioFileService) CopyFile(ctx context.Context,
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	srcKey, err := s.parseMinioFilePath(srcPath)
	if err != nil {
		return "", fmt.Errorf("minio copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}

	ext := filepath.Ext(srcPath)
	destKey := s.prefixedObjectName("%d/%s/%s%s", tenantID, knowledgeID, uuid.New().String(), ext)

	_, err = s.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.bucketName, Object: destKey},
		minio.CopySrcOptions{Bucket: s.bucketName, Object: srcKey},
	)
	if err != nil {
		return "", fmt.Errorf("failed to copy file in MinIO: %w", err)
	}

	newPath := fmt.Sprintf("minio://%s/%s", s.bucketName, destKey)
	logger.Infof(ctx, "Copied MinIO object %s to %s", srcPath, newPath)
	return newPath, nil
}

// SaveBytes saves bytes data to MinIO and returns the file path.
func (s *minioFileService) SaveBytes(ctx context.Context, data []byte, tenantID uint64, fileName string, temp bool) (string, error) {
	filePath, err := s.ReserveBytesPath(tenantID, fileName, temp)
	if err != nil {
		return "", err
	}
	if err := s.CommitBytesAtPath(ctx, data, filePath); err != nil {
		return "", err
	}
	return filePath, nil
}

// GetFileURL returns a presigned download URL for the file
func (s *minioFileService) GetFileURL(ctx context.Context, filePath string) (string, error) {
	objectName, err := s.parseMinioFilePath(filePath)
	if err != nil {
		return "", err
	}
	presignedURL, err := s.client.PresignedGetObject(ctx, s.bucketName, objectName, 24*time.Hour, nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %w", err)
	}
	return presignedURL.String(), nil
}
