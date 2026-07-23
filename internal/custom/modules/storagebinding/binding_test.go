package storagebinding

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validS3Binding(t *testing.T) Binding {
	t.Helper()
	ref, err := CredentialProfileReference(CredentialScopeTenant, ProviderS3, "default")
	require.NoError(t, err)
	binding, err := Normalize(Binding{
		Provider:        ProviderS3,
		Endpoint:        "https://s3.old.example.test",
		Region:          "cn-test-1",
		Bucket:          "documents",
		PathPrefix:      "/tenant-data/",
		UseSSL:          true,
		ForcePathStyle:  true,
		ConfigSource:    ConfigSourceTenant,
		CredentialScope: CredentialScopeTenant,
		CredentialRef:   ref,
	})
	require.NoError(t, err)
	return binding
}

func TestBindingJSONContainsIdentityButNeverCredentialMaterial(t *testing.T) {
	binding := validS3Binding(t)
	encoded, err := json.Marshal(binding)
	require.NoError(t, err)

	jsonText := string(encoded)
	require.NotContains(t, jsonText, "AKID-tenant-42")
	require.NotContains(t, jsonText, "super-secret-value")
	require.Contains(t, jsonText, binding.CredentialRef)
	require.Contains(t, jsonText, binding.Fingerprint)
	require.Equal(t, "storage-profile:v1:tenant:s3:default", binding.CredentialRef)
	require.Len(t, binding.Fingerprint, 64)
}

func TestBindingTamperingFailsClosed(t *testing.T) {
	original := validS3Binding(t)
	mutations := map[string]func(*Binding){
		"endpoint":   func(b *Binding) { b.Endpoint = "https://s3.new.example.test" },
		"region":     func(b *Binding) { b.Region = "cn-test-2" },
		"bucket":     func(b *Binding) { b.Bucket = "other-documents" },
		"prefix":     func(b *Binding) { b.PathPrefix = "other-prefix" },
		"credential": func(b *Binding) { b.CredentialRef = strings.Repeat("a", 64) },
		"path-style": func(b *Binding) { b.ForcePathStyle = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := original
			mutate(&changed)
			_, err := Normalize(changed)
			if name == "credential" {
				require.Error(t, err)
				return
			}
			require.ErrorIs(t, err, ErrBindingTampered)
		})
	}
}

func TestBindingRejectsMalformedTargetFields(t *testing.T) {
	ref, err := CredentialProfileReference(CredentialScopeTenant, ProviderS3, "default")
	require.NoError(t, err)
	tests := []Binding{
		{Provider: ProviderS3, Endpoint: "https://s3.example.test/path", Region: "region", Bucket: "bucket", UseSSL: true},
		{Provider: ProviderS3, Endpoint: "http://s3.example.test", Region: "region", Bucket: "bucket", UseSSL: true},
		{Provider: ProviderS3, Endpoint: "https://user:pass@s3.example.test", Region: "region", Bucket: "bucket", UseSSL: true},
		{Provider: ProviderS3, Endpoint: "https://s3.example.test", Region: "../region", Bucket: "bucket", UseSSL: true},
		{Provider: ProviderS3, Endpoint: "https://s3.example.test", Region: "region", Bucket: "../bucket", UseSSL: true},
	}
	for i := range tests {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			candidate := tests[i]
			candidate.ConfigSource = ConfigSourceTenant
			candidate.CredentialScope = CredentialScopeTenant
			candidate.CredentialRef = ref
			_, err := Normalize(candidate)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrInvalidBinding) || errors.Is(err, ErrUnsupported))
		})
	}
}

func TestNormalizeCanonicalizesEquivalentIdentity(t *testing.T) {
	ref, err := CredentialProfileReference(CredentialScopeTenant, ProviderS3, "default")
	require.NoError(t, err)
	a, err := Normalize(Binding{
		Provider: ProviderS3, Endpoint: "HTTPS://S3.EXAMPLE.TEST/", Region: "CN-TEST-1",
		Bucket: "documents", PathPrefix: "/tenant-data/", UseSSL: true,
		ConfigSource: ConfigSourceTenant, CredentialScope: CredentialScopeTenant, CredentialRef: ref,
	})
	require.NoError(t, err)
	b, err := Normalize(Binding{
		Version: CurrentVersion, Provider: ProviderS3, Endpoint: "https://s3.example.test",
		Region: "cn-test-1", Bucket: "documents", PathPrefix: "tenant-data", UseSSL: true,
		ConfigSource: ConfigSourceTenant, CredentialScope: CredentialScopeTenant, CredentialRef: ref,
	})
	require.NoError(t, err)
	require.Equal(t, a, b)
}
