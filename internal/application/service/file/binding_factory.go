package file

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type bindingCredentials struct {
	accessKey string
	secretKey string
}

// NewFileServiceForBinding reopens a previously persisted storage identity
// with current credential material. It performs no HeadBucket, BucketExists,
// CreateBucket or MakeBucket call. Any target or credential-identity mismatch
// fails closed.
func NewFileServiceForBinding(
	binding storagebinding.Binding,
	sec *types.StorageEngineConfig,
	injectedGlobal interfaces.FileService,
) (interfaces.FileService, error) {
	normalized, err := storagebinding.Normalize(binding)
	if err != nil {
		return nil, fmt.Errorf("resolve historical storage binding: %w", err)
	}

	if injectedGlobal != nil && serviceMatchesBinding(injectedGlobal, normalized) {
		return injectedGlobal, nil
	}
	if normalized.Provider == storagebinding.ProviderDummy {
		return nil, fmt.Errorf("resolve historical dummy binding: exact injected service is unavailable")
	}

	var service interfaces.FileService
	if normalized.Provider == storagebinding.ProviderLocal {
		service = NewLocalFileService(
			normalized.CanonicalLocalBase,
			strings.TrimSpace(os.Getenv("APP_EXTERNAL_URL")),
		)
		service = setStorageBindingIdentity(service, string(normalized.ConfigSource), string(normalized.CredentialScope))
		if !serviceMatchesBinding(service, normalized) {
			return nil, fmt.Errorf("resolve historical local binding: reconstructed service mismatch")
		}
		return service, nil
	}

	credentials, err := credentialsForBinding(normalized, sec)
	if err != nil {
		return nil, err
	}
	credentialRef, err := storagebinding.CredentialProfileReference(
		normalized.CredentialScope, normalized.Provider, "default",
	)
	if err != nil || credentialRef != normalized.CredentialRef {
		return nil, fmt.Errorf("resolve historical %s binding: credential profile mismatch", normalized.Provider)
	}

	switch normalized.Provider {
	case storagebinding.ProviderMinIO:
		endpoint, err := minioEndpointHost(normalized.Endpoint)
		if err != nil {
			return nil, err
		}
		service, err = newMinioClient(
			endpoint, credentials.accessKey, credentials.secretKey,
			normalized.Bucket, normalized.UseSSL, normalized.PathPrefix,
		)
	case storagebinding.ProviderCOS:
		var cosService *cosFileService
		cosService, err = newCosClient(
			normalized.Bucket, normalized.Region, credentials.accessKey, credentials.secretKey,
		)
		if cosService != nil {
			cosService.cosPathPrefix = normalized.PathPrefix
			service = cosService
		}
	case storagebinding.ProviderTOS:
		service, err = newTOSFileService(
			normalized.Endpoint, normalized.Region, credentials.accessKey, credentials.secretKey,
			normalized.Bucket, normalized.PathPrefix, "", "",
		)
	case storagebinding.ProviderS3:
		service, err = newS3Client(
			normalized.Endpoint, credentials.accessKey, credentials.secretKey,
			normalized.Bucket, normalized.Region, normalized.PathPrefix, normalized.ForcePathStyle,
		)
	case storagebinding.ProviderOSS:
		service, err = newOSSFileService(
			normalized.Endpoint, normalized.Region, credentials.accessKey, credentials.secretKey,
			normalized.Bucket, normalized.PathPrefix, "", "",
		)
	case storagebinding.ProviderKS3:
		service, err = newKS3FileService(
			normalized.Endpoint, normalized.Region, credentials.accessKey, credentials.secretKey,
			normalized.Bucket, normalized.PathPrefix,
		)
	case storagebinding.ProviderOBS:
		service, err = newObsFileService(
			normalized.Endpoint, normalized.Region, credentials.accessKey, credentials.secretKey,
			normalized.Bucket, normalized.PathPrefix, normalized.ProxyDomain,
		)
	default:
		return nil, fmt.Errorf("resolve historical storage binding: unsupported provider %q", normalized.Provider)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve historical %s binding: initialize client: %w", normalized.Provider, err)
	}
	service = setStorageBindingIdentity(service, string(normalized.ConfigSource), string(normalized.CredentialScope))
	if service == nil || !serviceMatchesBinding(service, normalized) {
		return nil, fmt.Errorf("resolve historical %s binding: reconstructed service mismatch", normalized.Provider)
	}
	return service, nil
}

func credentialsForBinding(binding storagebinding.Binding, sec *types.StorageEngineConfig) (bindingCredentials, error) {
	if binding.CredentialScope == storagebinding.CredentialScopeDirect {
		return bindingCredentials{}, fmt.Errorf("resolve historical %s binding: exact direct service is unavailable", binding.Provider)
	}
	if binding.CredentialScope == storagebinding.CredentialScopeTenant {
		if sec == nil {
			return bindingCredentials{}, fmt.Errorf("resolve historical %s binding: tenant storage config is unavailable", binding.Provider)
		}
		switch binding.Provider {
		case storagebinding.ProviderMinIO:
			if sec.MinIO != nil && strings.EqualFold(strings.TrimSpace(sec.MinIO.Mode), "remote") {
				return requiredCredentials("minio", sec.MinIO.AccessKeyID, sec.MinIO.SecretAccessKey)
			}
		case storagebinding.ProviderCOS:
			if sec.COS != nil {
				return requiredCredentials("cos", sec.COS.SecretID, sec.COS.SecretKey)
			}
		case storagebinding.ProviderTOS:
			if sec.TOS != nil {
				return requiredCredentials("tos", sec.TOS.AccessKey, sec.TOS.SecretKey)
			}
		case storagebinding.ProviderS3:
			if sec.S3 != nil {
				return requiredCredentials("s3", sec.S3.AccessKey, sec.S3.SecretKey)
			}
		case storagebinding.ProviderOSS:
			if sec.OSS != nil {
				return requiredCredentials("oss", sec.OSS.AccessKey, sec.OSS.SecretKey)
			}
		case storagebinding.ProviderKS3:
			if sec.KS3 != nil {
				return requiredCredentials("ks3", sec.KS3.AccessKey, sec.KS3.SecretKey)
			}
		case storagebinding.ProviderOBS:
			if sec.OBS != nil {
				return requiredCredentials("obs", sec.OBS.AccessKey, sec.OBS.SecretKey)
			}
		}
		return bindingCredentials{}, fmt.Errorf("resolve historical %s binding: matching tenant credentials are unavailable", binding.Provider)
	}
	if binding.CredentialScope != storagebinding.CredentialScopeGlobal {
		return bindingCredentials{}, fmt.Errorf("resolve historical %s binding: invalid credential scope", binding.Provider)
	}
	switch binding.Provider {
	case storagebinding.ProviderMinIO:
		return requiredCredentials("minio", os.Getenv("MINIO_ACCESS_KEY_ID"), os.Getenv("MINIO_SECRET_ACCESS_KEY"))
	case storagebinding.ProviderCOS:
		return requiredCredentials("cos", os.Getenv("COS_SECRET_ID"), os.Getenv("COS_SECRET_KEY"))
	case storagebinding.ProviderTOS:
		return requiredCredentials("tos", os.Getenv("TOS_ACCESS_KEY"), os.Getenv("TOS_SECRET_KEY"))
	case storagebinding.ProviderS3:
		return requiredCredentials("s3", os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"))
	case storagebinding.ProviderOSS:
		return requiredCredentials("oss", os.Getenv("OSS_ACCESS_KEY"), os.Getenv("OSS_SECRET_KEY"))
	case storagebinding.ProviderKS3:
		return requiredCredentials("ks3", os.Getenv("KS3_ACCESS_KEY"), os.Getenv("KS3_SECRET_KEY"))
	case storagebinding.ProviderOBS:
		return requiredCredentials("obs", os.Getenv("OBS_ACCESS_KEY"), os.Getenv("OBS_SECRET_KEY"))
	default:
		return bindingCredentials{}, fmt.Errorf("resolve historical storage binding: unsupported provider %q", binding.Provider)
	}
}

func requiredCredentials(provider, accessKey, secretKey string) (bindingCredentials, error) {
	accessKey, secretKey = strings.TrimSpace(accessKey), strings.TrimSpace(secretKey)
	if accessKey == "" || secretKey == "" {
		return bindingCredentials{}, fmt.Errorf("resolve historical %s binding: credentials are incomplete", provider)
	}
	return bindingCredentials{accessKey: accessKey, secretKey: secretKey}, nil
}

func minioEndpointHost(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("resolve historical minio binding: invalid endpoint")
	}
	return u.Host, nil
}

func serviceMatchesBinding(service interfaces.FileService, binding storagebinding.Binding) bool {
	provider, ok := service.(storagebinding.BindingProvider)
	if !ok || provider == nil {
		return false
	}
	probe, err := bindingProbePath(binding)
	if err != nil {
		return false
	}
	actual, err := provider.BindingForPath(probe)
	return err == nil && actual.Fingerprint == binding.Fingerprint
}

func bindingProbePath(binding storagebinding.Binding) (string, error) {
	const probeName = "__storage_binding_probe__"
	key, err := plannedfile.BuildKey(binding.PathPrefix, probeName)
	if err != nil {
		return "", err
	}
	switch binding.Provider {
	case storagebinding.ProviderLocal:
		return localScheme + probeName, nil
	case storagebinding.ProviderDummy:
		return dummyScheme + probeName, nil
	case storagebinding.ProviderCOS:
		return plannedfile.FormatRegionPath("cos", binding.Bucket, binding.Region, key)
	case storagebinding.ProviderMinIO, storagebinding.ProviderTOS, storagebinding.ProviderS3,
		storagebinding.ProviderOSS, storagebinding.ProviderKS3, storagebinding.ProviderOBS:
		return plannedfile.FormatBucketPath(string(binding.Provider), binding.Bucket, key)
	default:
		return "", fmt.Errorf("unsupported binding provider %q", binding.Provider)
	}
}
