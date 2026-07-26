// Package artifactstore owns the private object-storage namespace used by
// general/document-processing agent deliverables. Agent SDK working
// directories remain local and disposable; only verified final artifacts
// enter this store.
package artifactstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/objectnamespace"
	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	defaultBucket           = "weknora-artifacts"
	privateNamespaceExample = "weknora/__weknora_private_agent_artifacts_v1__/deployment"
)

type Store struct {
	service  interfaces.PrivateObjectFileService
	provider string
	bucket   string
	prefix   string
	uriRoot  string
}

func NewFromEnv() (*Store, error) {
	provider := artifactProvider()
	if provider == "" {
		return nil, fmt.Errorf("private artifact object storage is required: configure CUSTOM_GENERAL_AGENT_ARTIFACT_STORAGE_PROVIDER=minio|obs")
	}

	bucket := firstEnv(
		"CUSTOM_GENERAL_AGENT_ARTIFACT_BUCKET",
		"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_BUCKET",
	)
	prefix := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ARTIFACT_PATH_PREFIX"))
	if prefix == "" {
		return nil, fmt.Errorf(
			"private artifact path prefix is required: configure " +
				"CUSTOM_GENERAL_AGENT_ARTIFACT_PATH_PREFIX=" +
				privateNamespaceExample + "/<deployment>/namespace/<uuid>/",
		)
	}
	normalizedPrefix, err := objectnamespace.NormalizeAndValidate(
		prefix,
		objectnamespace.PurposeAgentArtifacts,
	)
	if err != nil {
		return nil, err
	}

	var base interfaces.FileService
	switch provider {
	case "minio":
		if bucket == "" {
			bucket = firstEnv("MINIO_BUCKET_NAME")
		}
		if bucket == "" {
			bucket = defaultBucket
		}
		endpoint := firstEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_MINIO_ENDPOINT",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_ENDPOINT",
			"MINIO_ENDPOINT",
		)
		accessKey := firstEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_MINIO_ACCESS_KEY_ID",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_ACCESS_KEY_ID",
			"MINIO_ACCESS_KEY_ID",
		)
		secretKey := firstEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_MINIO_SECRET_ACCESS_KEY",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_SECRET_ACCESS_KEY",
			"MINIO_SECRET_ACCESS_KEY",
		)
		if endpoint == "" || accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("incomplete private MinIO artifact configuration")
		}
		base, err = filesvc.NewMinioFileServiceWithPathPrefix(
			endpoint,
			accessKey,
			secretKey,
			bucket,
			envBool("CUSTOM_GENERAL_AGENT_ARTIFACT_MINIO_USE_SSL",
				envBool("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_USE_SSL", false)),
			normalizedPrefix,
		)
	case "obs":
		if bucket == "" {
			bucket = firstEnv("OBS_BUCKET_NAME")
		}
		if bucket == "" {
			return nil, fmt.Errorf("private OBS artifact bucket is required")
		}
		endpoint := firstEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_OBS_ENDPOINT",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_ENDPOINT",
			"OBS_ENDPOINT",
		)
		region := firstEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_OBS_REGION",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_REGION",
			"OBS_REGION",
		)
		if region == "" {
			region = "cn-north-4"
		}
		accessKey := firstEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_OBS_ACCESS_KEY",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_ACCESS_KEY",
			"OBS_ACCESS_KEY",
		)
		secretKey := firstEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_OBS_SECRET_KEY",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_SECRET_KEY",
			"OBS_SECRET_KEY",
		)
		if endpoint == "" || accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("incomplete private OBS artifact configuration")
		}
		base, err = filesvc.NewObsFileService(
			endpoint,
			region,
			accessKey,
			secretKey,
			bucket,
			normalizedPrefix,
		)
	default:
		return nil, fmt.Errorf("unsupported private artifact provider %q; only minio and obs are allowed", provider)
	}
	if err != nil {
		return nil, err
	}
	privateService, ok := base.(interfaces.PrivateObjectFileService)
	if !ok {
		return nil, fmt.Errorf("artifact provider %q does not implement private object commits", provider)
	}
	uriRoot := provider + "://" + bucket + "/"
	if normalizedPrefix != "" {
		uriRoot += normalizedPrefix + "/"
	}
	return &Store{
		service:  privateService,
		provider: provider,
		bucket:   bucket,
		prefix:   normalizedPrefix,
		uriRoot:  uriRoot,
	}, nil
}

// validatePrivateNamespacePrefix remains as a narrow package-level seam for
// the artifact-store tests. All namespace rules live in objectnamespace so
// ordinary knowledge objects and original inputs cannot drift to weaker rules.
func validatePrivateNamespacePrefix(prefix string) error {
	_, err := objectnamespace.NormalizeAndValidate(
		prefix,
		objectnamespace.PurposeAgentArtifacts,
	)
	return err
}

func (s *Store) CheckConnectivity(ctx context.Context) error {
	if s == nil || s.service == nil {
		return fmt.Errorf("private artifact object storage is unavailable")
	}
	return s.service.CheckConnectivity(ctx)
}

func (s *Store) Provider() string {
	if s == nil {
		return ""
	}
	return s.provider
}

func (s *Store) Bucket() string {
	if s == nil {
		return ""
	}
	return s.bucket
}

func (s *Store) Prefix() string {
	if s == nil {
		return ""
	}
	return s.prefix
}

func (s *Store) Owns(filePath string) bool {
	return s != nil && s.uriRoot != "" && strings.HasPrefix(strings.TrimSpace(filePath), s.uriRoot)
}

func (s *Store) Reserve(
	tenantID uint64,
	sessionID string,
	runID string,
	artifactID string,
	fileName string,
) (string, error) {
	if s == nil || s.service == nil {
		return "", fmt.Errorf("private artifact object storage is unavailable")
	}
	if tenantID == 0 {
		return "", fmt.Errorf("artifact tenant is required")
	}
	for label, value := range map[string]string{
		"session ID":  strings.TrimSpace(sessionID),
		"run ID":      strings.TrimSpace(runID),
		"artifact ID": strings.TrimSpace(artifactID),
	} {
		if err := plannedfile.ValidateSegment(label, value); err != nil {
			return "", err
		}
	}
	ext := strings.ToLower(path.Ext(strings.TrimSpace(fileName)))
	if len(ext) > 20 {
		ext = ""
	}
	objectName := strings.TrimSpace(artifactID) + ext
	return s.service.ReservePrivateObjectPath(
		"tenant",
		strconv.FormatUint(tenantID, 10),
		"session",
		strings.TrimSpace(sessionID),
		"run",
		strings.TrimSpace(runID),
		"artifact",
		strings.TrimSpace(artifactID),
		objectName,
	)
}

func (s *Store) CommitAndVerify(
	ctx context.Context,
	data []byte,
	filePath string,
	contentType string,
	sha256 string,
) error {
	if !s.Owns(filePath) {
		return fmt.Errorf("artifact path is outside the private object namespace")
	}
	if err := s.service.CommitPrivateObjectAtPath(ctx, data, filePath, contentType, sha256); err != nil {
		return err
	}
	if err := s.service.VerifyPrivateObject(ctx, filePath, int64(len(data)), sha256); err != nil {
		return err
	}
	return nil
}

func (s *Store) Verify(ctx context.Context, filePath string, size int64, sha256 string) error {
	if !s.Owns(filePath) {
		return fmt.Errorf("artifact path is outside the private object namespace")
	}
	return s.service.VerifyPrivateObject(ctx, filePath, size, sha256)
}

func (s *Store) Open(ctx context.Context, filePath string) (io.ReadCloser, error) {
	if !s.Owns(filePath) {
		return nil, fmt.Errorf("artifact path is outside the private object namespace")
	}
	return s.service.GetFile(ctx, filePath)
}

func (s *Store) Delete(ctx context.Context, filePath string) error {
	if !s.Owns(filePath) {
		return fmt.Errorf("artifact path is outside the private object namespace")
	}
	return s.service.DeleteFile(ctx, filePath)
}

func artifactProvider() string {
	provider := strings.ToLower(firstEnv(
		"CUSTOM_GENERAL_AGENT_ARTIFACT_STORAGE_PROVIDER",
		"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_STORAGE_PROVIDER",
		"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_PROVIDER",
	))
	if provider != "" {
		return provider
	}
	storageType := strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE_TYPE")))
	if storageType == "minio" || storageType == "obs" {
		return storageType
	}
	if strings.TrimSpace(os.Getenv("MINIO_ENDPOINT")) != "" {
		return "minio"
	}
	return ""
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}
