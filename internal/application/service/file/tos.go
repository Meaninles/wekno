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
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"
)

// tosFileService implements the FileService interface for Volcengine TOS.
type tosFileService struct {
	client          *tos.ClientV2
	tempClient      *tos.ClientV2
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
	tosProvider = "tos"
	tosScheme   = tosProvider + "://"
)

var _ interfaces.PlannedFileService = (*tosFileService)(nil)

// NewTosFileService creates a TOS file service.
func NewTosFileService(endpoint, region, accessKey, secretKey, bucketName, pathPrefix string) (interfaces.FileService, error) {
	return NewTosFileServiceWithTempBucket(endpoint, region, accessKey, secretKey, bucketName, pathPrefix, "", "")
}

// NewTosFileServiceWithTempBucket creates a TOS file service with optional temp bucket.
func NewTosFileServiceWithTempBucket(endpoint, region, accessKey, secretKey, bucketName, pathPrefix, tempBucketName, tempRegion string) (interfaces.FileService, error) {
	svc, err := newTOSFileService(
		endpoint, region, accessKey, secretKey, bucketName, pathPrefix, tempBucketName, tempRegion,
	)
	if err != nil {
		return nil, err
	}
	if err := ensureTOSBucket(svc.client, bucketName); err != nil {
		return nil, err
	}
	if svc.tempClient != nil {
		if err := ensureTOSBucket(svc.tempClient, tempBucketName); err != nil {
			return nil, err
		}
	}
	return svc, nil
}

// newTOSFileService constructs clients without probing or provisioning any
// bucket. Historical binding resolution must remain read-only.
func newTOSFileService(
	endpoint, region, accessKey, secretKey, bucketName, pathPrefix, tempBucketName, tempRegion string,
) (*tosFileService, error) {
	client, err := tos.NewClientV2(
		endpoint,
		tos.WithRegion(region),
		tos.WithCredentials(tos.NewStaticCredentials(accessKey, secretKey)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize TOS client: %w", err)
	}

	var tempClient *tos.ClientV2
	if tempBucketName != "" {
		if tempRegion == "" {
			tempRegion = region
		}
		tempClient, err = tos.NewClientV2(
			endpoint,
			tos.WithRegion(tempRegion),
			tos.WithCredentials(tos.NewStaticCredentials(accessKey, secretKey)),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize TOS temp client: %w", err)
		}
	}

	credentialRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeDirect, storagebinding.ProviderTOS, "default",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to identify TOS credentials: %w", err)
	}
	return &tosFileService{
		client:          client,
		tempClient:      tempClient,
		endpoint:        endpoint,
		region:          region,
		tempRegion:      tempRegion,
		pathPrefix:      strings.Trim(pathPrefix, "/"),
		bucketName:      bucketName,
		tempBucketName:  tempBucketName,
		bindingSource:   "direct",
		credentialScope: "direct",
		credentialRef:   credentialRef,
	}, nil
}

func (s *tosFileService) ReserveFilePath(
	tenantID uint64, knowledgeID string, fileName string,
) (string, error) {
	key, err := plannedfile.FileKey(s.pathPrefix, tenantID, knowledgeID, fileName)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath(tosProvider, s.bucketName, key)
}

func (s *tosFileService) ReserveBytesPath(
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
		return plannedfile.FormatBucketPath(tosProvider, s.tempBucketName, key)
	}
	key, err := plannedfile.BytesKey(s.pathPrefix, tenantID, fileName, "exports")
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath(tosProvider, s.bucketName, key)
}

func (s *tosFileService) ReserveCopyPath(
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	srcKey, err := plannedfile.ParseBucketPath(srcPath, tosProvider, s.bucketName, "")
	if err != nil {
		return "", fmt.Errorf("tos copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}
	return s.ReserveFilePath(tenantID, knowledgeID, "copy"+path.Ext(srcKey))
}

func (s *tosFileService) plannedMainKey(filePath string) (string, error) {
	key, err := plannedfile.ParseBucketPath(filePath, tosProvider, s.bucketName, s.pathPrefix)
	if err != nil {
		return "", fmt.Errorf("planned TOS destination is not bound to this service: %w", err)
	}
	return key, nil
}

func (s *tosFileService) plannedBytesTarget(filePath string) (*tos.ClientV2, string, string, error) {
	if key, err := s.plannedMainKey(filePath); err == nil {
		return s.client, s.bucketName, key, nil
	}
	if s.tempClient != nil {
		key, err := plannedfile.ParseBucketPath(filePath, tosProvider, s.tempBucketName, "exports")
		if err == nil {
			return s.tempClient, s.tempBucketName, key, nil
		}
	}
	return nil, "", "", fmt.Errorf("planned TOS bytes destination is not bound to this service")
}

func (s *tosFileService) CommitFileAtPath(
	ctx context.Context, file *multipart.FileHeader, filePath string,
) error {
	if file == nil {
		return fmt.Errorf("planned TOS commit: file is nil")
	}
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("planned TOS commit: open upload: %w", err)
	}
	defer src.Close()
	ext := filepath.Ext(file.Filename)
	contentType := file.Header.Get("Content-Type")
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(ext)
	}
	_, err = s.client.PutObjectV2(ctx, &tos.PutObjectV2Input{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket: s.bucketName, Key: key, ContentType: contentType,
		},
		Content: src,
	})
	if err != nil {
		return fmt.Errorf("planned TOS commit: upload: %w", err)
	}
	return nil
}

func (s *tosFileService) commitReaderAtPath(
	ctx context.Context, reader io.ReadSeeker, _ int64, contentType, filePath string,
) error {
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = utils.GetContentTypeByExt(path.Ext(key))
	}
	_, err = s.client.PutObjectV2(ctx, &tos.PutObjectV2Input{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket: s.bucketName, Key: key, ContentType: contentType,
		},
		Content: reader,
	})
	if err != nil {
		return fmt.Errorf("planned TOS stream commit: %w", err)
	}
	return nil
}

func (s *tosFileService) CommitBytesAtPath(ctx context.Context, data []byte, filePath string) error {
	client, bucket, key, err := s.plannedBytesTarget(filePath)
	if err != nil {
		return err
	}
	_, err = client.PutObjectV2(ctx, &tos.PutObjectV2Input{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket: bucket, Key: key, ContentType: utils.GetContentTypeByExt(path.Ext(key)),
		},
		Content: bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("planned TOS bytes commit: upload: %w", err)
	}
	return nil
}

func (s *tosFileService) CommitCopyAtPath(ctx context.Context, srcPath string, dstPath string) error {
	srcKey, err := plannedfile.ParseBucketPath(srcPath, tosProvider, s.bucketName, "")
	if err != nil {
		return fmt.Errorf("planned TOS copy source: %w", err)
	}
	dstKey, err := s.plannedMainKey(dstPath)
	if err != nil {
		return err
	}
	_, err = s.client.CopyObject(ctx, &tos.CopyObjectInput{
		Bucket: s.bucketName, Key: dstKey, SrcBucket: s.bucketName, SrcKey: srcKey,
	})
	if err != nil {
		return fmt.Errorf("planned TOS copy commit: %w", err)
	}
	return nil
}

// CheckConnectivity verifies TOS is reachable by performing a HeadBucket request.
func (s *tosFileService) CheckConnectivity(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.client.HeadBucket(checkCtx, &tos.HeadBucketInput{
		Bucket: s.bucketName,
	})
	return err
}

// CheckTosConnectivity tests TOS connectivity using the provided credentials.
func CheckTosConnectivity(ctx context.Context, endpoint, region, accessKey, secretKey, bucketName string) error {
	client, err := tos.NewClientV2(
		endpoint,
		tos.WithRegion(region),
		tos.WithCredentials(tos.NewStaticCredentials(accessKey, secretKey)),
	)
	if err != nil {
		return fmt.Errorf("failed to initialize TOS client: %w", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = client.HeadBucket(checkCtx, &tos.HeadBucketInput{
		Bucket: bucketName,
	})
	return err
}

func ensureTOSBucket(client *tos.ClientV2, bucketName string) error {
	_, err := client.HeadBucket(context.Background(), &tos.HeadBucketInput{
		Bucket: bucketName,
	})
	if err == nil {
		return nil
	}

	var serverErr *tos.TosServerError
	if errors.As(err, &serverErr) && serverErr.StatusCode == 404 {
		_, createErr := client.CreateBucketV2(context.Background(), &tos.CreateBucketV2Input{
			Bucket: bucketName,
		})
		if createErr == nil {
			return nil
		}
		if errors.As(createErr, &serverErr) && serverErr.StatusCode == 409 {
			return nil
		}
		return fmt.Errorf("failed to create TOS bucket: %w", createErr)
	}

	return fmt.Errorf("failed to check TOS bucket: %w", err)
}

func joinTOSObjectKey(parts ...string) string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(part, "/")
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, "/")
}

func parseTOSFilePath(filePath string) (bucketName string, objectKey string, err error) {
	if !strings.HasPrefix(filePath, tosScheme) {
		return "", "", fmt.Errorf("invalid TOS file path: %s", filePath)
	}

	rest := strings.TrimPrefix(filePath, tosScheme)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid TOS file path: %s", filePath)
	}
	return parts[0], parts[1], nil
}

func (s *tosFileService) SaveFile(ctx context.Context, file *multipart.FileHeader, tenantID uint64, knowledgeID string) (string, error) {
	if file == nil {
		return "", fmt.Errorf("failed to save file to TOS: file is nil")
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

func (s *tosFileService) SaveBytes(ctx context.Context, data []byte, tenantID uint64, fileName string, temp bool) (string, error) {
	filePath, err := s.ReserveBytesPath(tenantID, fileName, temp)
	if err != nil {
		return "", err
	}
	if err := s.CommitBytesAtPath(ctx, data, filePath); err != nil {
		return "", err
	}
	return filePath, nil
}

// CopyFile copies an existing TOS object to a new knowledge-owned object using a
// server-side CopyObject (no data leaves TOS). The destination uses the same
// layout as SaveFile. Returns ErrCrossBackendCopy when srcPath is not a tos:// path.
func (s *tosFileService) CopyFile(ctx context.Context,
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	newPath, err := s.ReserveCopyPath(srcPath, tenantID, knowledgeID)
	if err != nil {
		return "", err
	}
	if err := s.CommitCopyAtPath(ctx, srcPath, newPath); err != nil {
		return "", err
	}
	logger.Infof(ctx, "Copied TOS object %s to %s", srcPath, newPath)
	return newPath, nil
}

func (s *tosFileService) clientForBoundBucket(bucket string) (*tos.ClientV2, error) {
	if bucket == s.bucketName {
		return s.client, nil
	}
	if s.tempClient != nil && bucket == s.tempBucketName {
		return s.tempClient, nil
	}
	return nil, fmt.Errorf("TOS bucket %q is not bound to this service", bucket)
}

func (s *tosFileService) GetFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	bucketName, objectName, err := parseTOSFilePath(filePath)
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

	output, err := client.GetObjectV2(ctx, &tos.GetObjectV2Input{
		Bucket: bucketName,
		Key:    objectName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get file from TOS: %w", err)
	}
	return output.Content, nil
}

func (s *tosFileService) DeleteFile(ctx context.Context, filePath string) error {
	bucketName, objectName, err := parseTOSFilePath(filePath)
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

	_, err = client.DeleteObjectV2(ctx, &tos.DeleteObjectV2Input{
		Bucket: bucketName,
		Key:    objectName,
	})
	if err != nil {
		return fmt.Errorf("failed to delete file from TOS: %w", err)
	}
	return nil
}

func (s *tosFileService) GetFileURL(ctx context.Context, filePath string) (string, error) {
	bucketName, objectName, err := parseTOSFilePath(filePath)
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

	output, err := client.PreSignedURL(&tos.PreSignedURLInput{
		HTTPMethod: enum.HttpMethodGet,
		Bucket:     bucketName,
		Key:        objectName,
		Expires:    int64((24 * time.Hour).Seconds()),
	})
	if err != nil {
		return "", fmt.Errorf("failed to generate TOS presigned URL: %w", err)
	}
	return output.SignedUrl, nil
}
