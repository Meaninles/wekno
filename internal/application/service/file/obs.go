package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type obsFileService struct {
	client          *s3.Client
	bucketName      string
	endpoint        string
	region          string
	pathPrefix      string
	proxyDomain     string
	bindingSource   string
	credentialScope string
	credentialRef   string
}

const obsProvider = "obs"

var _ interfaces.PlannedFileService = (*obsFileService)(nil)
var _ interfaces.PrivateObjectFileService = (*obsFileService)(nil)
var _ interfaces.StreamingPrivateObjectFileService = (*obsFileService)(nil)

type obsEndpointResolver struct {
	url string
}

func (r *obsEndpointResolver) ResolveEndpoint(region string, options s3.EndpointResolverOptions) (aws.Endpoint, error) {
	return aws.Endpoint{
		URL:               r.url,
		SigningRegion:     region,
		HostnameImmutable: true,
	}, nil
}

func NewObsFileService(
	endpoint, region, accessKeyID, secretAccessKey, bucketName string,
	pathPrefix string,
) (interfaces.FileService, error) {
	svc, err := newObsFileService(
		endpoint, region, accessKeyID, secretAccessKey, bucketName, pathPrefix,
		strings.TrimSuffix(os.Getenv("OBS_PROXY_DOMAIN"), "/"),
	)
	if err != nil {
		return nil, err
	}

	_, err = svc.client.HeadBucket(context.Background(), &s3.HeadBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		_, createErr := svc.client.CreateBucket(context.Background(), &s3.CreateBucketInput{
			Bucket: aws.String(bucketName),
		})
		if createErr != nil {
			fmt.Printf("Warning: bucket %s may not exist or cannot be created: %v\n", bucketName, createErr)
		}
	}
	return svc, nil
}

// newObsFileService constructs a client without probing or provisioning a
// bucket. Historical binding resolution must use this read-only constructor.
func newObsFileService(
	endpoint, region, accessKeyID, secretAccessKey, bucketName, pathPrefix, proxyDomain string,
) (*obsFileService, error) {
	client := s3.New(s3.Options{
		Region:           region,
		EndpointResolver: &obsEndpointResolver{url: endpoint},
		Credentials:      credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
		UsePathStyle:     true,
	})

	credentialRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeDirect, storagebinding.ProviderOBS, "default",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to identify OBS credentials: %w", err)
	}
	return &obsFileService{
		client:          client,
		bucketName:      bucketName,
		endpoint:        endpoint,
		region:          region,
		pathPrefix:      strings.Trim(pathPrefix, "/"),
		proxyDomain:     strings.TrimSuffix(proxyDomain, "/"),
		bindingSource:   "direct",
		credentialScope: "direct",
		credentialRef:   credentialRef,
	}, nil
}

func (s *obsFileService) ReserveFilePath(
	tenantID uint64, knowledgeID string, fileName string,
) (string, error) {
	key, err := plannedfile.FileKey(s.pathPrefix, tenantID, knowledgeID, fileName)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath(obsProvider, s.bucketName, key)
}

func (s *obsFileService) ReserveBytesPath(
	tenantID uint64, fileName string, temp bool,
) (string, error) {
	name, err := plannedfile.NewObjectName(fileName)
	if err != nil {
		return "", err
	}
	segments := []string{strconv.FormatUint(tenantID, 10), name}
	if temp {
		segments = append([]string{"temp"}, segments...)
	}
	key, err := plannedfile.BuildKey(s.pathPrefix, segments...)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath(obsProvider, s.bucketName, key)
}

func (s *obsFileService) ReserveCopyPath(
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	srcKey, err := plannedfile.ParseBucketPath(srcPath, obsProvider, s.bucketName, "")
	if err != nil {
		return "", fmt.Errorf("obs copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}
	return s.ReserveFilePath(tenantID, knowledgeID, "copy"+path.Ext(srcKey))
}

func (s *obsFileService) ReservePrivateObjectPath(segments ...string) (string, error) {
	key, err := plannedfile.BuildKey(s.pathPrefix, segments...)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath(obsProvider, s.bucketName, key)
}

func (s *obsFileService) CommitPrivateObjectAtPath(
	ctx context.Context,
	data []byte,
	filePath string,
	contentType string,
	sha256 string,
) error {
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucketName),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(contentType),
		Metadata: map[string]string{
			"sha256": strings.ToLower(strings.TrimSpace(sha256)),
		},
		// Deliberately no ACL. Production must also enable bucket-default
		// server-side encryption (SSE-OBS/KMS), which applies to this upload.
	})
	if err != nil {
		return fmt.Errorf("private OBS object commit: %w", err)
	}
	return nil
}

func (s *obsFileService) CommitPrivateObjectStreamAtPath(
	ctx context.Context,
	reader io.Reader,
	size int64,
	filePath string,
	contentType string,
	sha256 string,
) error {
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	if reader == nil || size < 0 {
		return fmt.Errorf("private OBS stream commit: invalid source")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucketName),
		Key:           aws.String(key),
		Body:          reader,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
		Metadata: map[string]string{
			"sha256": strings.ToLower(strings.TrimSpace(sha256)),
		},
	})
	if err != nil {
		return fmt.Errorf("private OBS stream commit: %w", err)
	}
	return nil
}

func (s *obsFileService) VerifyPrivateObject(
	ctx context.Context,
	filePath string,
	size int64,
	sha256 string,
) error {
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("verify private OBS object: %w", err)
	}
	if head.ContentLength == nil || *head.ContentLength != size {
		got := int64(-1)
		if head.ContentLength != nil {
			got = *head.ContentLength
		}
		return fmt.Errorf("verify private OBS object: size mismatch: got %d want %d", got, size)
	}
	wantSHA := strings.ToLower(strings.TrimSpace(sha256))
	gotSHA := strings.ToLower(strings.TrimSpace(head.Metadata["sha256"]))
	if wantSHA == "" || gotSHA != wantSHA {
		return fmt.Errorf("verify private OBS object: sha256 metadata mismatch")
	}
	return nil
}

func (s *obsFileService) plannedMainKey(filePath string) (string, error) {
	key, err := plannedfile.ParseBucketPath(filePath, obsProvider, s.bucketName, s.pathPrefix)
	if err != nil {
		return "", fmt.Errorf("planned OBS destination is not bound to this service: %w", err)
	}
	return key, nil
}

func (s *obsFileService) CommitFileAtPath(
	ctx context.Context, file *multipart.FileHeader, filePath string,
) error {
	if file == nil {
		return fmt.Errorf("planned OBS commit: file is nil")
	}
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("planned OBS commit: open upload: %w", err)
	}
	defer src.Close()
	contentType := file.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName), Key: aws.String(key), Body: src,
		ContentLength: aws.Int64(file.Size), ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("planned OBS commit: upload: %w", err)
	}
	return nil
}

func (s *obsFileService) commitReaderAtPath(
	ctx context.Context, reader io.ReadSeeker, size int64, contentType, filePath string,
) error {
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName), Key: aws.String(key), Body: reader,
		ContentLength: aws.Int64(size), ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("planned OBS stream commit: %w", err)
	}
	return nil
}

func (s *obsFileService) CommitBytesAtPath(ctx context.Context, data []byte, filePath string) error {
	key, err := s.plannedMainKey(filePath)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName), Key: aws.String(key), Body: bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("application/octet-stream"),
		ACL: "public-read",
	})
	if err != nil {
		return fmt.Errorf("planned OBS bytes commit: upload: %w", err)
	}
	return nil
}

func (s *obsFileService) CommitCopyAtPath(ctx context.Context, srcPath string, dstPath string) error {
	srcKey, err := plannedfile.ParseBucketPath(srcPath, obsProvider, s.bucketName, "")
	if err != nil {
		return fmt.Errorf("planned OBS copy source: %w", err)
	}
	dstKey, err := s.plannedMainKey(dstPath)
	if err != nil {
		return err
	}
	_, err = s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(s.bucketName), CopySource: aws.String(s.bucketName + "/" + srcKey),
		Key: aws.String(dstKey),
	})
	if err != nil {
		return fmt.Errorf("planned OBS copy commit: %w", err)
	}
	return nil
}

func CheckObsConnectivity(ctx context.Context, endpoint, region, accessKey, secretKey, bucketName string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client := s3.New(s3.Options{
		Region:           region,
		EndpointResolver: &obsEndpointResolver{url: endpoint},
		Credentials:      credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		UsePathStyle:     true,
	})

	_, err := client.HeadBucket(checkCtx, &s3.HeadBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return fmt.Errorf("OBS connectivity check failed: %w", err)
	}
	return nil
}

func (s *obsFileService) CheckConnectivity(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := s.client.HeadBucket(checkCtx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucketName),
	})
	if err != nil {
		return fmt.Errorf("OBS connectivity check failed: %w", err)
	}
	return nil
}

func (s *obsFileService) parseObsFilePath(filePath string) (string, error) {
	if strings.HasPrefix(filePath, obsProvider+"://") {
		key, err := plannedfile.ParseBucketPath(filePath, obsProvider, s.bucketName, "")
		if err != nil {
			return "", fmt.Errorf("invalid OBS file path: %w", err)
		}
		return key, nil
	}
	prefix := s.getPrifix()

	if strings.HasPrefix(filePath, prefix) {
		rest := strings.TrimPrefix(filePath, prefix)
		// With proxy domain: path is {prefix}/{objectKey} (no bucket name)
		if s.proxyDomain != "" {
			rest = strings.TrimPrefix(rest, "/")
			if err := plannedfile.ValidateKey(rest, ""); err == nil {
				return rest, nil
			}
			return "", fmt.Errorf("invalid OBS file path: %s", filePath)
		}
		// Without proxy domain: path is {prefix}/{bucketName}/{objectKey}
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 && parts[0] == s.bucketName && parts[1] != "" {
			return parts[1], nil
		}
		return "", fmt.Errorf("invalid OBS file path: %s", filePath)
	}
	if strings.Contains(filePath, "://") {
		return "", fmt.Errorf("invalid OBS file provider")
	}
	if err := plannedfile.ValidateKey(filePath, ""); err != nil {
		return "", fmt.Errorf("invalid OBS object key: %w", err)
	}
	return filePath, nil
}

func (s *obsFileService) getPrifix() string {
	if s.proxyDomain != "" {
		return s.proxyDomain + "/"
	}
	return "obs://"
}

func (s *obsFileService) legacyOBSPath(filePath string) (string, error) {
	if s.proxyDomain == "" {
		return filePath, nil
	}
	key, err := plannedfile.ParseBucketPath(filePath, obsProvider, s.bucketName, s.pathPrefix)
	if err != nil {
		return "", err
	}
	return s.proxyDomain + "/" + key, nil
}

func (s *obsFileService) SaveFile(ctx context.Context,
	file *multipart.FileHeader, tenantID uint64, knowledgeID string,
) (string, error) {
	if file == nil {
		return "", fmt.Errorf("failed to save file to OBS: file is nil")
	}
	filePath, err := s.ReserveFilePath(tenantID, knowledgeID, file.Filename)
	if err != nil {
		return "", err
	}
	if err := s.CommitFileAtPath(ctx, file, filePath); err != nil {
		return "", err
	}
	return s.legacyOBSPath(filePath)
}

func (s *obsFileService) GetFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	objectKey, err := s.parseObsFilePath(filePath)
	if err != nil {
		return nil, err
	}

	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get file from OBS: %w", err)
	}

	return output.Body, nil
}

func (s *obsFileService) DeleteFile(ctx context.Context, filePath string) error {
	objectKey, err := s.parseObsFilePath(filePath)
	if err != nil {
		return err
	}

	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return fmt.Errorf("failed to delete file from OBS: %w", err)
	}

	return nil
}

func (s *obsFileService) GetFileURL(ctx context.Context, filePath string) (string, error) {
	if strings.HasPrefix(filePath, "http://") || strings.HasPrefix(filePath, "https://") {
		return filePath, nil
	}

	objectKey, err := s.parseObsFilePath(filePath)
	if err != nil {
		return "", err
	}

	if s.proxyDomain != "" {
		return s.proxyDomain + "/" + strings.TrimPrefix(objectKey, "/"), nil
	}

	return fmt.Sprintf("%s/%s/%s", s.endpoint, s.bucketName, strings.TrimPrefix(objectKey, "/")), nil
}

// CopyFile copies an existing OBS object to a new knowledge-owned object using a
// server-side CopyObject (OBS is S3-compatible). The destination uses the same
// layout as SaveFile. Returns ErrCrossBackendCopy when srcPath does not belong
// to this OBS service.
func (s *obsFileService) CopyFile(ctx context.Context,
	srcPath string, tenantID uint64, knowledgeID string,
) (string, error) {
	// Reject paths that do not use this service's prefix (proxy domain or obs://).
	// parseObsFilePath falls back to returning the raw input for unknown prefixes,
	// so guard explicitly here to detect cross-backend sources.
	canonicalSource := srcPath
	if !strings.HasPrefix(srcPath, obsProvider+"://") {
		if !strings.HasPrefix(srcPath, s.getPrifix()) {
			return "", fmt.Errorf("obs copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
		}
		srcKey, err := s.parseObsFilePath(srcPath)
		if err != nil {
			return "", fmt.Errorf("obs copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
		}
		canonicalSource, err = plannedfile.FormatBucketPath(obsProvider, s.bucketName, srcKey)
		if err != nil {
			return "", err
		}
	}
	newPath, err := s.ReserveCopyPath(canonicalSource, tenantID, knowledgeID)
	if err != nil {
		return "", fmt.Errorf("obs copy rejected source %q: %w", srcPath, ErrCrossBackendCopy)
	}
	if err := s.CommitCopyAtPath(ctx, canonicalSource, newPath); err != nil {
		return "", err
	}
	legacyPath, err := s.legacyOBSPath(newPath)
	if err != nil {
		return "", err
	}
	logger.Infof(ctx, "Copied OBS object %s to %s", srcPath, legacyPath)
	return legacyPath, nil
}

func (s *obsFileService) SaveBytes(ctx context.Context, data []byte, tenantID uint64, fileName string, temp bool) (string, error) {
	filePath, err := s.ReserveBytesPath(tenantID, fileName, temp)
	if err != nil {
		return "", err
	}
	if err := s.CommitBytesAtPath(ctx, data, filePath); err != nil {
		return "", err
	}
	return s.legacyOBSPath(filePath)
}
