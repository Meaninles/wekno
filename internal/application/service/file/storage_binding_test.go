package file

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

func requireBinding(t *testing.T, service interfaces.FileService, filePath string) storagebinding.Binding {
	t.Helper()
	provider, ok := service.(storagebinding.BindingProvider)
	require.True(t, ok)
	binding, err := provider.BindingForPath(filePath)
	require.NoError(t, err)
	require.NoError(t, binding.VerifyFingerprint())
	return binding
}

func TestLocalBindingDistinguishesRootsAndRemountAtSamePath(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "storage")
	require.NoError(t, os.Mkdir(base, 0o755))
	original := NewLocalFileService(base, "")
	path := "local://7/knowledge/file.pdf"
	originalBinding := requireBinding(t, original, path)

	otherBase := filepath.Join(parent, "other")
	require.NoError(t, os.Mkdir(otherBase, 0o755))
	other := NewLocalFileService(otherBase, "")
	otherBinding := requireBinding(t, other, path)
	require.NotEqual(t, originalBinding.CanonicalLocalBase, otherBinding.CanonicalLocalBase)
	require.NotEqual(t, originalBinding.Fingerprint, otherBinding.Fingerprint)

	resolved, err := NewFileServiceForBinding(originalBinding, nil, original)
	require.NoError(t, err)
	require.Same(t, original, resolved)

	oldRoot := filepath.Join(parent, "storage-old")
	require.NoError(t, os.Rename(base, oldRoot))
	require.NoError(t, os.Mkdir(base, 0o755))
	replacement := NewLocalFileService(base, "")
	replacementBinding := requireBinding(t, replacement, path)
	require.Equal(t, originalBinding.CanonicalLocalBase, replacementBinding.CanonicalLocalBase)
	require.NotEqual(t, originalBinding.LocalRootIdentity, replacementBinding.LocalRootIdentity)
	require.NotEqual(t, originalBinding.Fingerprint, replacementBinding.Fingerprint)

	_, err = NewFileServiceForBinding(originalBinding, nil, replacement)
	require.ErrorContains(t, err, "reconstructed service mismatch")
}

func TestHistoricalBindingUsesOldEndpointWithSameBucket(t *testing.T) {
	var oldRequests, currentRequests atomic.Int32
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer oldServer.Close()
	currentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer currentServer.Close()

	oldService, err := newS3Client(oldServer.URL, "same-ak", "old-secret", "documents", "region-1", "tenant", true)
	require.NoError(t, err)
	setStorageBindingIdentity(oldService, storageBindingSourceTenant, storageBindingSourceTenant)
	oldBinding := requireBinding(t, oldService, "s3://documents/tenant/7/file.pdf")

	currentService, err := newS3Client(currentServer.URL, "same-ak", "current-secret", "documents", "region-1", "tenant", true)
	require.NoError(t, err)
	setStorageBindingIdentity(currentService, storageBindingSourceTenant, storageBindingSourceTenant)

	sec := &types.StorageEngineConfig{S3: &types.S3EngineConfig{
		AccessKey: "same-ak", SecretKey: "current-secret",
	}}
	resolved, err := NewFileServiceForBinding(oldBinding, sec, currentService)
	require.NoError(t, err)
	require.Zero(t, oldRequests.Load(), "binding resolution must not probe a bucket")
	require.Zero(t, currentRequests.Load(), "binding resolution must not touch the current endpoint")
	require.NoError(t, resolved.DeleteFile(context.Background(), "s3://documents/tenant/7/file.pdf"))
	require.Equal(t, int32(1), oldRequests.Load())
	require.Zero(t, currentRequests.Load())
}

func TestHistoricalBindingAllowsCredentialRotationWithinSameProfile(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	service, err := newS3Client(server.URL, "old-ak", "old-secret", "documents", "region-1", "tenant", true)
	require.NoError(t, err)
	setStorageBindingIdentity(service, storageBindingSourceTenant, storageBindingSourceTenant)
	binding := requireBinding(t, service, "s3://documents/tenant/7/file.pdf")
	resolved, err := NewFileServiceForBinding(binding, &types.StorageEngineConfig{S3: &types.S3EngineConfig{
		AccessKey: "new-ak", SecretKey: "new-secret",
	}}, nil)
	require.NoError(t, err)
	require.Zero(t, requests.Load())
	require.NoError(t, resolved.DeleteFile(context.Background(), "s3://documents/tenant/7/file.pdf"))
	require.Equal(t, int32(1), requests.Load())
}

func TestBindingFactoryForEveryRemoteProviderDoesNotProbeOrCreateBucket(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Errorf("binding resolver issued unexpected storage request: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	const accessKey = "tenant-ak"
	const secretKey = "tenant-secret"

	type fixture struct {
		name    string
		service interfaces.FileService
		path    string
		config  *types.StorageEngineConfig
	}
	fixtures := make([]fixture, 0, 7)

	minioService, err := newMinioClient(u.Host, accessKey, secretKey, "documents", false, "tenant")
	require.NoError(t, err)
	fixtures = append(fixtures, fixture{"minio", minioService, "minio://documents/tenant/7/file.pdf", &types.StorageEngineConfig{
		MinIO: &types.MinIOEngineConfig{Mode: "remote", AccessKeyID: accessKey, SecretAccessKey: secretKey},
	}})
	cosService, err := newCosClient("documents-12345", "ap-test-1", accessKey, secretKey)
	require.NoError(t, err)
	cosService.cosPathPrefix = "tenant"
	fixtures = append(fixtures, fixture{"cos", cosService, "cos://documents-12345/ap-test-1/tenant/7/file.pdf", &types.StorageEngineConfig{
		COS: &types.COSEngineConfig{SecretID: accessKey, SecretKey: secretKey},
	}})
	tosService, err := newTOSFileService(server.URL, "region-1", accessKey, secretKey, "documents", "tenant", "", "")
	require.NoError(t, err)
	fixtures = append(fixtures, fixture{"tos", tosService, "tos://documents/tenant/7/file.pdf", &types.StorageEngineConfig{
		TOS: &types.TOSEngineConfig{AccessKey: accessKey, SecretKey: secretKey},
	}})
	s3Service, err := newS3Client(server.URL, accessKey, secretKey, "documents", "region-1", "tenant", true)
	require.NoError(t, err)
	fixtures = append(fixtures, fixture{"s3", s3Service, "s3://documents/tenant/7/file.pdf", &types.StorageEngineConfig{
		S3: &types.S3EngineConfig{AccessKey: accessKey, SecretKey: secretKey},
	}})
	ossService, err := newOSSFileService(server.URL, "region-1", accessKey, secretKey, "documents", "tenant", "", "")
	require.NoError(t, err)
	fixtures = append(fixtures, fixture{"oss", ossService, "oss://documents/tenant/7/file.pdf", &types.StorageEngineConfig{
		OSS: &types.OSSEngineConfig{AccessKey: accessKey, SecretKey: secretKey},
	}})
	ks3Service, err := newKS3FileService(server.URL, "region-1", accessKey, secretKey, "documents", "tenant")
	require.NoError(t, err)
	fixtures = append(fixtures, fixture{"ks3", ks3Service, "ks3://documents/tenant/7/file.pdf", &types.StorageEngineConfig{
		KS3: &types.KS3EngineConfig{AccessKey: accessKey, SecretKey: secretKey},
	}})
	obsService, err := newObsFileService(server.URL, "region-1", accessKey, secretKey, "documents", "tenant", "https://cdn.example.test")
	require.NoError(t, err)
	fixtures = append(fixtures, fixture{"obs", obsService, "obs://documents/tenant/7/file.pdf", &types.StorageEngineConfig{
		OBS: &types.OBSEngineConfig{AccessKey: accessKey, SecretKey: secretKey},
	}})

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			setStorageBindingIdentity(fixture.service, storageBindingSourceTenant, storageBindingSourceTenant)
			binding := requireBinding(t, fixture.service, fixture.path)
			resolved, err := NewFileServiceForBinding(binding, fixture.config, nil)
			require.NoError(t, err)
			require.True(t, serviceMatchesBinding(resolved, binding))
		})
	}
	require.Zero(t, requests.Load())
}

func TestTempBucketsAndLegacyProviderPathsHaveExactBindings(t *testing.T) {
	const accessKey = "tenant-ak"
	const secretKey = "tenant-secret"

	cosServiceRaw, err := NewCosFileServiceWithTempBucket(
		"main-12345", "ap-main-1", accessKey, secretKey, "tenant",
		"temp-12345", "ap-temp-1",
	)
	require.NoError(t, err)
	cosService := cosServiceRaw.(*cosFileService)
	setStorageBindingIdentity(cosService, storageBindingSourceTenant, storageBindingSourceTenant)
	cosMain := requireBinding(t, cosService, "cos://main-12345/ap-main-1/tenant/7/file.pdf")
	cosTemp := requireBinding(t, cosService, "cos://temp-12345/ap-temp-1/exports/7/file.pdf")
	cosTempHTTPS := requireBinding(t, cosService, "https://temp-12345.cos.ap-temp-1.myqcloud.com/exports/7/file.pdf")
	require.Equal(t, "temp-12345", cosTemp.Bucket)
	require.Equal(t, "ap-temp-1", cosTemp.Region)
	require.Equal(t, "exports", cosTemp.PathPrefix)
	require.Equal(t, cosTemp.Fingerprint, cosTempHTTPS.Fingerprint)
	require.NotEqual(t, cosMain.Fingerprint, cosTemp.Fingerprint)
	_, err = cosService.BindingForPath("https://other-12345.cos.ap-main-1.myqcloud.com/tenant/7/file.pdf")
	require.Error(t, err)

	tosService, err := newTOSFileService(
		"https://tos.example.test", "region-main", accessKey, secretKey, "main-bucket", "tenant",
		"temp-bucket", "region-temp",
	)
	require.NoError(t, err)
	setStorageBindingIdentity(tosService, storageBindingSourceTenant, storageBindingSourceTenant)
	tosTemp := requireBinding(t, tosService, "tos://temp-bucket/exports/7/file.pdf")
	require.Equal(t, "temp-bucket", tosTemp.Bucket)
	require.Equal(t, "region-temp", tosTemp.Region)
	require.Equal(t, "exports", tosTemp.PathPrefix)

	ossService, err := newOSSFileService(
		"https://oss.example.test", "region-main", accessKey, secretKey, "main-bucket", "tenant",
		"temp-bucket", "region-temp",
	)
	require.NoError(t, err)
	setStorageBindingIdentity(ossService, storageBindingSourceTenant, storageBindingSourceTenant)
	ossTemp := requireBinding(t, ossService, "oss://temp-bucket/exports/7/file.pdf")
	require.Equal(t, "temp-bucket", ossTemp.Bucket)
	require.Equal(t, "region-temp", ossTemp.Region)
	require.Equal(t, "exports", ossTemp.PathPrefix)
}

func TestOBSProxyBindingRejectsOtherDomainsAndRawKeys(t *testing.T) {
	service, err := newObsFileService(
		"https://obs.example.test", "region-1", "tenant-ak", "tenant-secret",
		"documents", "tenant", "https://cdn.example.test",
	)
	require.NoError(t, err)
	setStorageBindingIdentity(service, storageBindingSourceTenant, storageBindingSourceTenant)
	canonical := requireBinding(t, service, "obs://documents/tenant/7/file.pdf")
	proxied := requireBinding(t, service, "https://cdn.example.test/tenant/7/file.pdf")
	require.Equal(t, canonical.Fingerprint, proxied.Fingerprint)
	require.Equal(t, "https://cdn.example.test", proxied.ProxyDomain)

	for _, path := range []string{
		"https://other.example.test/tenant/7/file.pdf",
		"tenant/7/file.pdf",
		"obs://other/tenant/7/file.pdf",
	} {
		t.Run(fmt.Sprintf("reject-%x", path), func(t *testing.T) {
			_, err := service.BindingForPath(path)
			require.Error(t, err)
		})
	}
}
