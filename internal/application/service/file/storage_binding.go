package file

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
)

var (
	_ storagebinding.BindingProvider = (*localFileService)(nil)
	_ storagebinding.BindingProvider = (*DummyFileService)(nil)
	_ storagebinding.BindingProvider = (*minioFileService)(nil)
	_ storagebinding.BindingProvider = (*cosFileService)(nil)
	_ storagebinding.BindingProvider = (*tosFileService)(nil)
	_ storagebinding.BindingProvider = (*s3FileService)(nil)
	_ storagebinding.BindingProvider = (*ossFileService)(nil)
	_ storagebinding.BindingProvider = (*ks3FileService)(nil)
	_ storagebinding.BindingProvider = (*obsFileService)(nil)
)

func bindingSource(value string) storagebinding.ConfigSource {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case storageBindingSourceTenant:
		return storagebinding.ConfigSourceTenant
	case storageBindingSourceGlobal:
		return storagebinding.ConfigSourceGlobal
	default:
		return storagebinding.ConfigSourceDirect
	}
}

func bindingCredentialScope(value string, local bool) storagebinding.CredentialScope {
	if local {
		return storagebinding.CredentialScopeNone
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case storageBindingSourceTenant:
		return storagebinding.CredentialScopeTenant
	case storageBindingSourceGlobal:
		return storagebinding.CredentialScopeGlobal
	default:
		return storagebinding.CredentialScopeDirect
	}
}

func endpointUsesSSL(endpoint string, fallback bool) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Scheme == "" {
		return fallback
	}
	return strings.EqualFold(u.Scheme, "https")
}

func normalizedBinding(binding storagebinding.Binding) (storagebinding.Binding, error) {
	result, err := storagebinding.Normalize(binding)
	if err != nil {
		return storagebinding.Binding{}, fmt.Errorf("derive storage binding: %w", err)
	}
	return result, nil
}

func (s *localFileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if s == nil {
		return storagebinding.Binding{}, fmt.Errorf("local binding: nil service")
	}
	if _, _, err := s.plannedDestination(filePath); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("local binding: %w", err)
	}
	base, err := filepath.Abs(filepath.Clean(s.baseDir))
	if err != nil {
		return storagebinding.Binding{}, fmt.Errorf("local binding: canonical base: %w", err)
	}
	rootIdentity, err := localRootIdentity(base)
	if err != nil {
		return storagebinding.Binding{}, fmt.Errorf("local binding: %w", err)
	}
	return normalizedBinding(storagebinding.Binding{
		Provider: storagebinding.ProviderLocal, CanonicalLocalBase: base, LocalRootIdentity: rootIdentity,
		ConfigSource: bindingSource(s.bindingSource), CredentialScope: storagebinding.CredentialScopeNone,
	})
}

func (s *DummyFileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if s == nil {
		return storagebinding.Binding{}, fmt.Errorf("dummy binding: nil service")
	}
	if _, err := parseDummyPlannedPath(filePath); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("dummy binding: %w", err)
	}
	return normalizedBinding(storagebinding.Binding{
		Provider: storagebinding.ProviderDummy, ConfigSource: bindingSource(s.bindingSource),
		CredentialScope: storagebinding.CredentialScopeNone,
	})
}

func (s *minioFileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if s == nil {
		return storagebinding.Binding{}, fmt.Errorf("minio binding: nil service")
	}
	key, err := s.parseMinioFilePath(filePath)
	if err != nil {
		return storagebinding.Binding{}, fmt.Errorf("minio binding: %w", err)
	}
	if err := plannedfile.ValidateKey(key, s.pathPrefix); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("minio binding: %w", err)
	}
	return normalizedBinding(storagebinding.Binding{
		Provider: storagebinding.ProviderMinIO, Endpoint: s.endpoint, Bucket: s.bucketName,
		PathPrefix: s.pathPrefix, UseSSL: s.useSSL, ConfigSource: bindingSource(s.bindingSource),
		CredentialScope: bindingCredentialScope(s.credentialScope, false), CredentialRef: s.credentialRef,
	})
}

func (s *s3FileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if s == nil {
		return storagebinding.Binding{}, fmt.Errorf("s3 binding: nil service")
	}
	key, err := s.parseS3FilePath(filePath)
	if err != nil {
		return storagebinding.Binding{}, fmt.Errorf("s3 binding: %w", err)
	}
	if err := plannedfile.ValidateKey(key, s.pathPrefix); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("s3 binding: %w", err)
	}
	return normalizedBinding(storagebinding.Binding{
		Provider: storagebinding.ProviderS3, Endpoint: s.endpoint, Region: s.region,
		Bucket: s.bucketName, PathPrefix: s.pathPrefix,
		UseSSL: endpointUsesSSL(s.endpoint, true), ForcePathStyle: s.usePathStyle,
		ConfigSource:    bindingSource(s.bindingSource),
		CredentialScope: bindingCredentialScope(s.credentialScope, false), CredentialRef: s.credentialRef,
	})
}

func (s *cosFileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if s == nil {
		return storagebinding.Binding{}, fmt.Errorf("cos binding: nil service")
	}
	client, key, err := s.resolveCosObject(filePath)
	if err != nil {
		return storagebinding.Binding{}, fmt.Errorf("cos binding: %w", err)
	}
	bucket, region, prefix := s.bucketName, s.region, s.cosPathPrefix
	if s.tempClient != nil && client == s.tempClient {
		bucket, region, prefix = s.tempBucketName, s.tempRegion, "exports"
	}
	if err := plannedfile.ValidateKey(key, prefix); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("cos binding: %w", err)
	}
	return normalizedBinding(storagebinding.Binding{
		Provider: storagebinding.ProviderCOS,
		Endpoint: storagebinding.COSCanonicalEndpoint(bucket, region), Region: region,
		Bucket: bucket, PathPrefix: prefix, UseSSL: true,
		ConfigSource:    bindingSource(s.bindingSource),
		CredentialScope: bindingCredentialScope(s.credentialScope, false), CredentialRef: s.credentialRef,
	})
}

func (s *tosFileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if s == nil {
		return storagebinding.Binding{}, fmt.Errorf("tos binding: nil service")
	}
	bucket, key, err := parseTOSFilePath(filePath)
	if err != nil {
		return storagebinding.Binding{}, fmt.Errorf("tos binding: %w", err)
	}
	if _, err := s.clientForBoundBucket(bucket); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("tos binding: %w", err)
	}
	region, prefix := s.region, s.pathPrefix
	if bucket == s.tempBucketName && s.tempClient != nil {
		region, prefix = s.tempRegion, "exports"
	}
	if err := plannedfile.ValidateKey(key, prefix); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("tos binding: %w", err)
	}
	return normalizedBinding(storagebinding.Binding{
		Provider: storagebinding.ProviderTOS, Endpoint: s.endpoint, Region: region,
		Bucket: bucket, PathPrefix: prefix, UseSSL: endpointUsesSSL(s.endpoint, true),
		ConfigSource:    bindingSource(s.bindingSource),
		CredentialScope: bindingCredentialScope(s.credentialScope, false), CredentialRef: s.credentialRef,
	})
}

func (s *ossFileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if s == nil {
		return storagebinding.Binding{}, fmt.Errorf("oss binding: nil service")
	}
	bucket, key, err := parseOssFilePath(filePath)
	if err != nil {
		return storagebinding.Binding{}, fmt.Errorf("oss binding: %w", err)
	}
	if _, err := s.clientForBoundBucket(bucket); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("oss binding: %w", err)
	}
	region, prefix := s.region, s.pathPrefix
	if bucket == s.tempBucketName && s.tempClient != nil {
		region, prefix = s.tempRegion, "exports"
	}
	if err := plannedfile.ValidateKey(key, prefix); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("oss binding: %w", err)
	}
	return normalizedBinding(storagebinding.Binding{
		Provider: storagebinding.ProviderOSS, Endpoint: s.endpoint, Region: region,
		Bucket: bucket, PathPrefix: prefix, UseSSL: endpointUsesSSL(s.endpoint, true),
		ConfigSource:    bindingSource(s.bindingSource),
		CredentialScope: bindingCredentialScope(s.credentialScope, false), CredentialRef: s.credentialRef,
	})
}

func (s *ks3FileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if s == nil {
		return storagebinding.Binding{}, fmt.Errorf("ks3 binding: nil service")
	}
	bucket, key, err := parseKS3FilePath(filePath)
	if err != nil || bucket != s.bucketName {
		return storagebinding.Binding{}, fmt.Errorf("ks3 binding: path is not bound to bucket %q", s.bucketName)
	}
	if err := plannedfile.ValidateKey(key, s.pathPrefix); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("ks3 binding: %w", err)
	}
	return normalizedBinding(storagebinding.Binding{
		Provider: storagebinding.ProviderKS3, Endpoint: s.endpoint, Region: s.region,
		Bucket: s.bucketName, PathPrefix: s.pathPrefix, UseSSL: endpointUsesSSL(s.endpoint, true),
		ConfigSource:    bindingSource(s.bindingSource),
		CredentialScope: bindingCredentialScope(s.credentialScope, false), CredentialRef: s.credentialRef,
	})
}

func (s *obsFileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if s == nil {
		return storagebinding.Binding{}, fmt.Errorf("obs binding: nil service")
	}
	canonical := strings.HasPrefix(filePath, obsProvider+"://")
	proxied := s.proxyDomain != "" && strings.HasPrefix(filePath, s.proxyDomain+"/")
	if !canonical && !proxied {
		return storagebinding.Binding{}, fmt.Errorf("obs binding: path is not an exact canonical or proxy path")
	}
	key, err := s.parseObsFilePath(filePath)
	if err != nil {
		return storagebinding.Binding{}, fmt.Errorf("obs binding: %w", err)
	}
	if err := plannedfile.ValidateKey(key, s.pathPrefix); err != nil {
		return storagebinding.Binding{}, fmt.Errorf("obs binding: %w", err)
	}
	return normalizedBinding(storagebinding.Binding{
		Provider: storagebinding.ProviderOBS, Endpoint: s.endpoint, Region: s.region,
		Bucket: s.bucketName, PathPrefix: s.pathPrefix, ProxyDomain: s.proxyDomain,
		UseSSL: endpointUsesSSL(s.endpoint, true), ForcePathStyle: true,
		ConfigSource:    bindingSource(s.bindingSource),
		CredentialScope: bindingCredentialScope(s.credentialScope, false), CredentialRef: s.credentialRef,
	})
}
