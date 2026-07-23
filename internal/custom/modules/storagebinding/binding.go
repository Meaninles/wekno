// Package storagebinding defines the durable, non-secret identity of a
// storage target. A binding deliberately excludes credentials: historical
// objects are reopened with the current credential material in the same
// logical provider profile while retaining the endpoint, region, bucket and
// key namespace that actually own the object.
package storagebinding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
)

const CurrentVersion = 1

type ProviderName string

const (
	ProviderLocal ProviderName = "local"
	ProviderDummy ProviderName = "dummy"
	ProviderMinIO ProviderName = "minio"
	ProviderCOS   ProviderName = "cos"
	ProviderTOS   ProviderName = "tos"
	ProviderS3    ProviderName = "s3"
	ProviderOSS   ProviderName = "oss"
	ProviderKS3   ProviderName = "ks3"
	ProviderOBS   ProviderName = "obs"
)

type ConfigSource string

const (
	ConfigSourceDirect ConfigSource = "direct"
	ConfigSourceTenant ConfigSource = "tenant"
	ConfigSourceGlobal ConfigSource = "global"
)

type CredentialScope string

const (
	CredentialScopeNone   CredentialScope = "none"
	CredentialScopeDirect CredentialScope = "direct"
	CredentialScopeTenant CredentialScope = "tenant"
	CredentialScopeGlobal CredentialScope = "global"
)

var (
	ErrInvalidBinding  = errors.New("invalid storage binding")
	ErrBindingTampered = errors.New("storage binding fingerprint mismatch")
	ErrUnsupported     = errors.New("unsupported storage binding")
)

// Binding is safe to persist in task_pending_ops. Do not add access keys,
// secret keys, tokens, signed URLs or other credential material to this type.
type Binding struct {
	Version            int             `json:"version"`
	Provider           ProviderName    `json:"provider"`
	Endpoint           string          `json:"endpoint,omitempty"`
	Region             string          `json:"region,omitempty"`
	Bucket             string          `json:"bucket,omitempty"`
	PathPrefix         string          `json:"path_prefix,omitempty"`
	CanonicalLocalBase string          `json:"canonical_local_base,omitempty"`
	LocalRootIdentity  string          `json:"local_root_identity,omitempty"`
	ProxyDomain        string          `json:"proxy_domain,omitempty"`
	UseSSL             bool            `json:"use_ssl,omitempty"`
	ForcePathStyle     bool            `json:"force_path_style,omitempty"`
	ConfigSource       ConfigSource    `json:"config_source"`
	CredentialScope    CredentialScope `json:"credential_scope"`
	CredentialRef      string          `json:"credential_ref,omitempty"`
	Fingerprint        string          `json:"fingerprint"`
}

// BindingProvider derives the exact target identity for a provider path and
// rejects paths that are not owned by that service instance.
type BindingProvider interface {
	BindingForPath(filePath string) (Binding, error)
}

type fingerprintPayload struct {
	Version            int             `json:"version"`
	Provider           ProviderName    `json:"provider"`
	Endpoint           string          `json:"endpoint,omitempty"`
	Region             string          `json:"region,omitempty"`
	Bucket             string          `json:"bucket,omitempty"`
	PathPrefix         string          `json:"path_prefix,omitempty"`
	CanonicalLocalBase string          `json:"canonical_local_base,omitempty"`
	LocalRootIdentity  string          `json:"local_root_identity,omitempty"`
	ProxyDomain        string          `json:"proxy_domain,omitempty"`
	UseSSL             bool            `json:"use_ssl,omitempty"`
	ForcePathStyle     bool            `json:"force_path_style,omitempty"`
	ConfigSource       ConfigSource    `json:"config_source"`
	CredentialScope    CredentialScope `json:"credential_scope"`
	CredentialRef      string          `json:"credential_ref,omitempty"`
}

func (b Binding) payload() fingerprintPayload {
	return fingerprintPayload{
		Version: b.Version, Provider: b.Provider, Endpoint: b.Endpoint,
		Region: b.Region, Bucket: b.Bucket, PathPrefix: b.PathPrefix,
		CanonicalLocalBase: b.CanonicalLocalBase, ProxyDomain: b.ProxyDomain,
		LocalRootIdentity: b.LocalRootIdentity,
		UseSSL:            b.UseSSL, ForcePathStyle: b.ForcePathStyle,
		ConfigSource: b.ConfigSource, CredentialScope: b.CredentialScope,
		CredentialRef: b.CredentialRef,
	}
}

func (b Binding) ComputeFingerprint() (string, error) {
	payload, err := json.Marshal(b.payload())
	if err != nil {
		return "", fmt.Errorf("%w: encode fingerprint: %v", ErrInvalidBinding, err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func (b Binding) VerifyFingerprint() error {
	normalized, err := normalizeFields(b)
	if err != nil {
		return err
	}
	expected, err := normalized.ComputeFingerprint()
	if err != nil {
		return err
	}
	if b.Fingerprint == "" || !strings.EqualFold(strings.TrimSpace(b.Fingerprint), expected) {
		return ErrBindingTampered
	}
	return nil
}

func (b Binding) Validate() error {
	_, err := normalizeFields(b)
	return err
}

func (b Binding) Normalize() (Binding, error) { return Normalize(b) }

// Normalize canonicalizes all non-secret target fields and either computes a
// new fingerprint or verifies a supplied one. Thus decoding and normalizing a
// modified persisted payload always fails closed.
func Normalize(b Binding) (Binding, error) {
	originalFingerprint := strings.ToLower(strings.TrimSpace(b.Fingerprint))
	b.Fingerprint = ""
	normalized, err := normalizeFields(b)
	if err != nil {
		return Binding{}, err
	}
	fingerprint, err := normalized.ComputeFingerprint()
	if err != nil {
		return Binding{}, err
	}
	if originalFingerprint != "" && originalFingerprint != fingerprint {
		return Binding{}, ErrBindingTampered
	}
	normalized.Fingerprint = fingerprint
	return normalized, nil
}

func normalizeFields(b Binding) (Binding, error) {
	if b.Version == 0 {
		b.Version = CurrentVersion
	}
	if b.Version != CurrentVersion {
		return Binding{}, fmt.Errorf("%w: version %d", ErrUnsupported, b.Version)
	}
	b.Provider = ProviderName(strings.ToLower(strings.TrimSpace(string(b.Provider))))
	b.Region = strings.ToLower(strings.TrimSpace(b.Region))
	b.Bucket = strings.TrimSpace(b.Bucket)
	rawPrefix := strings.TrimSpace(b.PathPrefix)
	normalizedPrefix, err := plannedfile.NormalizePrefix(rawPrefix)
	if err != nil {
		return Binding{}, fmt.Errorf("%w: %v", ErrInvalidBinding, err)
	}
	b.PathPrefix = normalizedPrefix
	b.ProxyDomain = strings.TrimRight(strings.TrimSpace(b.ProxyDomain), "/")
	b.CanonicalLocalBase = strings.TrimSpace(b.CanonicalLocalBase)
	b.LocalRootIdentity = strings.TrimSpace(b.LocalRootIdentity)
	b.ConfigSource = ConfigSource(strings.ToLower(strings.TrimSpace(string(b.ConfigSource))))
	b.CredentialScope = CredentialScope(strings.ToLower(strings.TrimSpace(string(b.CredentialScope))))
	b.CredentialRef = strings.ToLower(strings.TrimSpace(b.CredentialRef))

	if !validProvider(b.Provider) {
		return Binding{}, fmt.Errorf("%w: provider %q", ErrInvalidBinding, b.Provider)
	}
	if !validConfigSource(b.ConfigSource) {
		return Binding{}, fmt.Errorf("%w: config source %q", ErrInvalidBinding, b.ConfigSource)
	}
	if !validCredentialScope(b.CredentialScope) {
		return Binding{}, fmt.Errorf("%w: credential scope %q", ErrInvalidBinding, b.CredentialScope)
	}
	switch b.Provider {
	case ProviderLocal:
		if b.CanonicalLocalBase == "" || b.LocalRootIdentity == "" || b.Bucket != "" || b.Endpoint != "" || b.Region != "" || b.ProxyDomain != "" {
			return Binding{}, fmt.Errorf("%w: malformed local target", ErrInvalidBinding)
		}
		if len(b.LocalRootIdentity) > 256 || strings.ContainsAny(b.LocalRootIdentity, "\x00\r\n") {
			return Binding{}, fmt.Errorf("%w: malformed local root identity", ErrInvalidBinding)
		}
		absolute, err := filepath.Abs(filepath.Clean(b.CanonicalLocalBase))
		if err != nil {
			return Binding{}, fmt.Errorf("%w: canonical local base: %v", ErrInvalidBinding, err)
		}
		b.CanonicalLocalBase = filepath.Clean(absolute)
		b.PathPrefix = ""
		b.CredentialScope = CredentialScopeNone
		b.CredentialRef = ""
	case ProviderDummy:
		if b.Endpoint != "" || b.Region != "" || b.Bucket != "" || b.PathPrefix != "" || b.CanonicalLocalBase != "" || b.LocalRootIdentity != "" || b.ProxyDomain != "" {
			return Binding{}, fmt.Errorf("%w: malformed dummy target", ErrInvalidBinding)
		}
		b.CredentialScope = CredentialScopeNone
		b.CredentialRef = ""
	case ProviderMinIO:
		if b.Bucket == "" {
			return Binding{}, fmt.Errorf("%w: minio bucket is empty", ErrInvalidBinding)
		}
		if err := plannedfile.ValidateSegment("bucket", b.Bucket); err != nil {
			return Binding{}, fmt.Errorf("%w: %v", ErrInvalidBinding, err)
		}
		endpoint, err := NormalizeEndpoint(b.Endpoint, b.UseSSL)
		if err != nil {
			return Binding{}, err
		}
		b.Endpoint = endpoint
	case ProviderS3:
		if b.Bucket == "" || b.Region == "" {
			return Binding{}, fmt.Errorf("%w: s3 bucket/region is empty", ErrInvalidBinding)
		}
		if err := validateBucketRegion(b.Bucket, b.Region); err != nil {
			return Binding{}, err
		}
		if b.Endpoint != "" {
			endpoint, err := NormalizeEndpoint(b.Endpoint, b.UseSSL)
			if err != nil {
				return Binding{}, err
			}
			b.Endpoint = endpoint
		}
	case ProviderCOS:
		if b.Bucket == "" || b.Region == "" {
			return Binding{}, fmt.Errorf("%w: cos bucket/region is empty", ErrInvalidBinding)
		}
		if err := validateBucketRegion(b.Bucket, b.Region); err != nil {
			return Binding{}, err
		}
		canonical := COSCanonicalEndpoint(b.Bucket, b.Region)
		if strings.TrimSpace(b.Endpoint) == "" {
			b.Endpoint = canonical
		} else {
			endpoint, err := NormalizeEndpoint(b.Endpoint, true)
			if err != nil || endpoint != canonical {
				return Binding{}, fmt.Errorf("%w: non-canonical COS endpoint", ErrInvalidBinding)
			}
			b.Endpoint = endpoint
		}
		b.UseSSL = true
	case ProviderTOS, ProviderOSS, ProviderKS3, ProviderOBS:
		if b.Endpoint == "" || b.Bucket == "" || b.Region == "" {
			return Binding{}, fmt.Errorf("%w: %s endpoint/region/bucket is empty", ErrInvalidBinding, b.Provider)
		}
		if err := validateBucketRegion(b.Bucket, b.Region); err != nil {
			return Binding{}, err
		}
		endpoint, err := NormalizeEndpoint(b.Endpoint, b.UseSSL)
		if err != nil {
			return Binding{}, err
		}
		b.Endpoint = endpoint
	default:
		return Binding{}, fmt.Errorf("%w: provider %q", ErrInvalidBinding, b.Provider)
	}

	if b.Provider != ProviderLocal && b.Provider != ProviderDummy && b.CredentialScope == CredentialScopeNone {
		return Binding{}, fmt.Errorf("%w: remote provider has no credential scope", ErrInvalidBinding)
	}
	if b.Provider != ProviderLocal && b.Provider != ProviderDummy {
		expected, err := CredentialProfileReference(b.CredentialScope, b.Provider, "default")
		if err != nil || b.CredentialRef != expected {
			return Binding{}, fmt.Errorf("%w: malformed credential profile reference", ErrInvalidBinding)
		}
	}
	if b.ProxyDomain != "" {
		if b.Provider != ProviderOBS {
			return Binding{}, fmt.Errorf("%w: proxy domain is only valid for OBS", ErrInvalidBinding)
		}
		proxy, err := normalizeHTTPURL(b.ProxyDomain)
		if err != nil {
			return Binding{}, fmt.Errorf("%w: proxy domain: %v", ErrInvalidBinding, err)
		}
		b.ProxyDomain = proxy
	}
	b.Fingerprint = ""
	return b, nil
}

func validProvider(provider ProviderName) bool {
	switch provider {
	case ProviderLocal, ProviderDummy, ProviderMinIO, ProviderCOS, ProviderTOS,
		ProviderS3, ProviderOSS, ProviderKS3, ProviderOBS:
		return true
	default:
		return false
	}
}

func validConfigSource(source ConfigSource) bool {
	switch source {
	case ConfigSourceDirect, ConfigSourceTenant, ConfigSourceGlobal:
		return true
	default:
		return false
	}
}

func validCredentialScope(scope CredentialScope) bool {
	switch scope {
	case CredentialScopeNone, CredentialScopeDirect, CredentialScopeTenant, CredentialScopeGlobal:
		return true
	default:
		return false
	}
}

func validateBucketRegion(bucket, region string) error {
	if err := plannedfile.ValidateSegment("bucket", bucket); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBinding, err)
	}
	if err := plannedfile.ValidateSegment("region", region); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBinding, err)
	}
	return nil
}

// NormalizeEndpoint accepts a URL or host[:port], removes trailing slashes,
// lowercases the authority and rejects userinfo/query/fragment/path data.
func NormalizeEndpoint(raw string, useSSL bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: endpoint is empty", ErrInvalidBinding)
	}
	if !strings.Contains(raw, "://") {
		scheme := "http"
		if useSSL {
			scheme = "https"
		}
		raw = scheme + "://" + raw
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("%w: invalid endpoint %q", ErrInvalidBinding, raw)
		}
		expectsHTTPS := strings.EqualFold(u.Scheme, "https")
		if expectsHTTPS != useSSL {
			return "", fmt.Errorf("%w: endpoint scheme and use_ssl disagree", ErrInvalidBinding)
		}
	}
	return normalizeHTTPURL(raw)
}

func normalizeHTTPURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: invalid endpoint %q", ErrInvalidBinding, raw)
	}
	if p := strings.Trim(u.EscapedPath(), "/"); p != "" {
		return "", fmt.Errorf("%w: endpoint must not contain a path", ErrInvalidBinding)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path, u.RawPath = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

func COSCanonicalEndpoint(bucket, region string) string {
	return "https://" + strings.ToLower(strings.TrimSpace(bucket)) + ".cos." +
		strings.ToLower(strings.TrimSpace(region)) + ".myqcloud.com"
}

// CredentialProfileReference identifies a logical credential slot, never a
// key value. Rotating both halves of a key pair in the same tenant/global
// provider slot therefore keeps historical objects reopenable. A different
// scope, provider, or explicitly named slot still fails closed.
func CredentialProfileReference(scope CredentialScope, provider ProviderName, slot string) (string, error) {
	scope = CredentialScope(strings.ToLower(strings.TrimSpace(string(scope))))
	provider = ProviderName(strings.ToLower(strings.TrimSpace(string(provider))))
	slot = strings.ToLower(strings.TrimSpace(slot))
	if !validCredentialScope(scope) || scope == CredentialScopeNone || !validProvider(provider) ||
		provider == ProviderLocal || provider == ProviderDummy {
		return "", fmt.Errorf("%w: invalid credential profile scope/provider", ErrInvalidBinding)
	}
	if slot == "" {
		slot = "default"
	}
	if err := plannedfile.ValidateSegment("credential slot", slot); err != nil {
		return "", fmt.Errorf("%w: invalid credential profile slot", ErrInvalidBinding)
	}
	return "storage-profile:v1:" + string(scope) + ":" + string(provider) + ":" + slot, nil
}
