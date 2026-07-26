package file

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/objectnamespace"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// NewReadOnlyFileServiceFromStorageConfig builds the current configured
// provider identity without issuing Head/BucketExists/Create/Make requests.
// It is intended for legacy ownership backfill and exact historical routing,
// never for connectivity checks or provisioning.
func NewReadOnlyFileServiceFromStorageConfig(
	provider string,
	sec *types.StorageEngineConfig,
	localBaseDir string,
) (interfaces.FileService, string, error) {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" && sec != nil {
		p = strings.ToLower(strings.TrimSpace(sec.DefaultProvider))
	}
	if p == "" {
		return nil, "", fmt.Errorf("empty provider")
	}
	if localBaseDir == "" {
		localBaseDir = strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR"))
	}
	if localBaseDir == "" {
		localBaseDir = "/data/files"
	}

	var (
		service interfaces.FileService
		err     error
	)
	switch p {
	case "local":
		baseDir := localBaseDir
		source := storageBindingSourceGlobal
		if sec != nil && sec.Local != nil {
			rawPrefix := strings.TrimSpace(sec.Local.PathPrefix)
			prefix := strings.Trim(rawPrefix, "/\\")
			if prefix != "" {
				candidate := filepath.Join(baseDir, prefix)
				safeBaseDir, safeErr := secutils.SafePathUnderBase(baseDir, candidate)
				if safeErr != nil {
					return nil, p, fmt.Errorf("invalid local storage prefix: %w", safeErr)
				}
				baseDir = safeBaseDir
			}
			source = storageBindingSourceTenant
		}
		service = NewLocalFileService(baseDir, strings.TrimSpace(os.Getenv("APP_EXTERNAL_URL")))
		return setStorageBindingIdentity(service, source, "none"), p, nil

	case "minio":
		var cfg *types.MinIOEngineConfig
		if sec != nil {
			cfg = sec.MinIO
		}
		if cfg == nil {
			if !strings.EqualFold(strings.TrimSpace(os.Getenv("STORAGE_TYPE")), "minio") {
				return nil, p, fmt.Errorf("missing minio config")
			}
			cfg = &types.MinIOEngineConfig{Mode: "docker"}
		}
		endpoint, accessKey, secretKey := "", "", ""
		credentialScope := storageBindingSourceGlobal
		if strings.EqualFold(strings.TrimSpace(cfg.Mode), "remote") {
			endpoint, accessKey, secretKey = cfg.Endpoint, cfg.AccessKeyID, cfg.SecretAccessKey
			credentialScope = storageBindingSourceTenant
		} else {
			endpoint = os.Getenv("MINIO_ENDPOINT")
			accessKey = os.Getenv("MINIO_ACCESS_KEY_ID")
			secretKey = os.Getenv("MINIO_SECRET_ACCESS_KEY")
		}
		bucket := strings.TrimSpace(cfg.BucketName)
		if bucket == "" {
			bucket = strings.TrimSpace(os.Getenv("MINIO_BUCKET_NAME"))
		}
		if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(accessKey) == "" ||
			strings.TrimSpace(secretKey) == "" || bucket == "" {
			return nil, p, fmt.Errorf("incomplete minio config")
		}
		pathPrefix := strings.TrimSpace(cfg.PathPrefix)
		if pathPrefix == "" && !strings.EqualFold(strings.TrimSpace(cfg.Mode), "remote") {
			pathPrefix, err = objectnamespace.KnowledgePrefixFromEnv("minio")
			if err != nil {
				return nil, p, err
			}
		}
		service, err = newMinioClient(
			strings.TrimSpace(endpoint), strings.TrimSpace(accessKey), strings.TrimSpace(secretKey),
			bucket, cfg.UseSSL, pathPrefix,
		)
		service = setStorageBindingIdentity(service, storageBindingSourceTenant, credentialScope)

	case "cos":
		if sec == nil || sec.COS == nil {
			return nil, p, fmt.Errorf("missing cos config")
		}
		cfg := sec.COS
		if cfg.SecretID == "" || cfg.SecretKey == "" || cfg.BucketName == "" || cfg.Region == "" {
			return nil, p, fmt.Errorf("incomplete cos config")
		}
		var svc *cosFileService
		svc, err = newCosClient(cfg.BucketName, cfg.Region, cfg.SecretID, cfg.SecretKey)
		if svc != nil {
			svc.cosPathPrefix = strings.TrimSpace(cfg.PathPrefix)
			if svc.cosPathPrefix == "" {
				svc.cosPathPrefix = "weknora"
			}
			service = svc
		}
		service = setStorageBindingIdentity(service, storageBindingSourceTenant, storageBindingSourceTenant)

	case "tos":
		if sec == nil || sec.TOS == nil {
			return nil, p, fmt.Errorf("missing tos config")
		}
		cfg := sec.TOS
		service, err = newTOSFileService(
			cfg.Endpoint, cfg.Region, cfg.AccessKey, cfg.SecretKey, cfg.BucketName, cfg.PathPrefix, "", "",
		)
		service = setStorageBindingIdentity(service, storageBindingSourceTenant, storageBindingSourceTenant)

	case "s3":
		if sec == nil || sec.S3 == nil {
			return nil, p, fmt.Errorf("missing s3 config")
		}
		cfg := sec.S3
		pathPrefix := strings.TrimSpace(cfg.PathPrefix)
		if pathPrefix == "" {
			pathPrefix = "weknora/"
		}
		if cfg.ForcePathStyle {
			service, err = newS3Client(
				cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.BucketName, cfg.Region, pathPrefix, true,
			)
		} else {
			service, err = newS3Client(
				cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.BucketName, cfg.Region, pathPrefix,
			)
		}
		service = setStorageBindingIdentity(service, storageBindingSourceTenant, storageBindingSourceTenant)

	case "oss":
		if sec == nil || sec.OSS == nil {
			return nil, p, fmt.Errorf("missing oss config")
		}
		cfg := sec.OSS
		pathPrefix := strings.TrimSpace(cfg.PathPrefix)
		if pathPrefix == "" {
			pathPrefix = "weknora/"
		}
		tempBucket, tempRegion := "", ""
		if cfg.UseTempBucket {
			tempBucket, tempRegion = cfg.TempBucketName, cfg.TempRegion
		}
		service, err = newOSSFileService(
			cfg.Endpoint, cfg.Region, cfg.AccessKey, cfg.SecretKey, cfg.BucketName,
			pathPrefix, tempBucket, tempRegion,
		)
		service = setStorageBindingIdentity(service, storageBindingSourceTenant, storageBindingSourceTenant)

	case "ks3":
		if sec == nil || sec.KS3 == nil {
			return nil, p, fmt.Errorf("missing ks3 config")
		}
		cfg := sec.KS3
		pathPrefix := strings.TrimSpace(cfg.PathPrefix)
		if pathPrefix == "" {
			pathPrefix = "weknora/"
		}
		service, err = newKS3FileService(
			cfg.Endpoint, cfg.Region, cfg.AccessKey, cfg.SecretKey, cfg.BucketName, pathPrefix,
		)
		service = setStorageBindingIdentity(service, storageBindingSourceTenant, storageBindingSourceTenant)

	case "obs":
		endpoint, region, accessKey, secretKey, bucket, pathPrefix := "", "", "", "", "", ""
		source := storageBindingSourceGlobal
		useTenant := sec != nil && sec.OBS != nil &&
			strings.TrimSpace(sec.OBS.Endpoint) != "" &&
			strings.TrimSpace(sec.OBS.AccessKey) != "" &&
			strings.TrimSpace(sec.OBS.SecretKey) != "" &&
			strings.TrimSpace(sec.OBS.BucketName) != ""
		if useTenant {
			cfg := sec.OBS
			endpoint, region, accessKey, secretKey = cfg.Endpoint, cfg.Region, cfg.AccessKey, cfg.SecretKey
			bucket, pathPrefix = cfg.BucketName, cfg.PathPrefix
			source = storageBindingSourceTenant
		} else {
			if !strings.EqualFold(strings.TrimSpace(os.Getenv("STORAGE_TYPE")), "obs") {
				return nil, p, fmt.Errorf("missing obs config")
			}
			endpoint, region = os.Getenv("OBS_ENDPOINT"), os.Getenv("OBS_REGION")
			accessKey, secretKey = os.Getenv("OBS_ACCESS_KEY"), os.Getenv("OBS_SECRET_KEY")
			bucket, pathPrefix = os.Getenv("OBS_BUCKET_NAME"), os.Getenv("OBS_PATH_PREFIX")
		}
		if strings.TrimSpace(region) == "" {
			region = "cn-north-4"
		}
		if strings.TrimSpace(pathPrefix) == "" {
			pathPrefix, err = objectnamespace.KnowledgePrefixFromEnv("obs")
			if err != nil {
				return nil, p, err
			}
		}
		service, err = newObsFileService(
			strings.TrimSpace(endpoint), strings.TrimSpace(region), strings.TrimSpace(accessKey),
			strings.TrimSpace(secretKey), strings.TrimSpace(bucket), strings.TrimSpace(pathPrefix),
			strings.TrimSpace(os.Getenv("OBS_PROXY_DOMAIN")),
		)
		service = setStorageBindingIdentity(service, source, source)

	default:
		return nil, p, fmt.Errorf("unsupported provider %q", p)
	}
	if err != nil {
		return nil, p, err
	}
	if service == nil {
		return nil, p, fmt.Errorf("provider %s returned nil service", p)
	}
	return service, p, nil
}
