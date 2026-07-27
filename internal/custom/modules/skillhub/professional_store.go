package skillhub

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/objectnamespace"
	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	defaultProfessionalSkillBucket = "weknora-original-inputs"
	professionalNamespaceExample   = "weknora/__weknora_private_professional_skills_v1__/deployment"
)

// ProfessionalObjectStore is the durable, shared backing store for uploaded
// professional skills. Implementations must reserve collision-resistant paths,
// commit private objects, and verify their length and digest before a database
// row is allowed to reference them.
type ProfessionalObjectStore interface {
	CheckConnectivity(context.Context) error
	Reserve(tenantID uint64, skillID, revisionID string) (string, error)
	CommitAndVerify(context.Context, []byte, string, string, string) error
	Verify(context.Context, string, int64, string) error
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}

type professionalObjectStore struct {
	service interfaces.PrivateObjectFileService
	uriRoot string
}

func newProfessionalObjectStoreFromEnv() (ProfessionalObjectStore, error) {
	provider := professionalStorageProvider()
	if provider == "" {
		return nil, fmt.Errorf(
			"professional skill object storage is required: configure " +
				"CUSTOM_SKILLHUB_PROFESSIONAL_STORAGE_PROVIDER=minio|obs",
		)
	}
	prefix := strings.TrimSpace(os.Getenv("CUSTOM_SKILLHUB_PROFESSIONAL_PATH_PREFIX"))
	if prefix == "" {
		return nil, fmt.Errorf(
			"professional skill object path prefix is required: configure " +
				"CUSTOM_SKILLHUB_PROFESSIONAL_PATH_PREFIX=" +
				professionalNamespaceExample + "/<deployment>/namespace/<uuid>/",
		)
	}
	normalizedPrefix, err := objectnamespace.NormalizeAndValidate(
		prefix,
		objectnamespace.PurposeProfessionalSkills,
	)
	if err != nil {
		return nil, err
	}

	bucket := firstProfessionalEnv("CUSTOM_SKILLHUB_PROFESSIONAL_BUCKET")
	var base interfaces.FileService
	switch provider {
	case "minio":
		if bucket == "" {
			bucket = firstProfessionalEnv("MINIO_BUCKET_NAME")
		}
		if bucket == "" {
			bucket = defaultProfessionalSkillBucket
		}
		endpoint := firstProfessionalEnv(
			"CUSTOM_SKILLHUB_PROFESSIONAL_MINIO_ENDPOINT",
			"MINIO_ENDPOINT",
		)
		accessKey := firstProfessionalEnv(
			"CUSTOM_SKILLHUB_PROFESSIONAL_MINIO_ACCESS_KEY_ID",
			"MINIO_ACCESS_KEY_ID",
		)
		secretKey := firstProfessionalEnv(
			"CUSTOM_SKILLHUB_PROFESSIONAL_MINIO_SECRET_ACCESS_KEY",
			"MINIO_SECRET_ACCESS_KEY",
		)
		if endpoint == "" || accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("incomplete professional skill MinIO configuration")
		}
		base, err = filesvc.NewMinioFileServiceWithPathPrefix(
			endpoint,
			accessKey,
			secretKey,
			bucket,
			professionalEnvBool("CUSTOM_SKILLHUB_PROFESSIONAL_MINIO_USE_SSL", false),
			normalizedPrefix,
		)
	case "obs":
		if bucket == "" {
			bucket = firstProfessionalEnv("OBS_BUCKET_NAME")
		}
		if bucket == "" {
			return nil, fmt.Errorf("professional skill OBS bucket is required")
		}
		endpoint := firstProfessionalEnv(
			"CUSTOM_SKILLHUB_PROFESSIONAL_OBS_ENDPOINT",
			"OBS_ENDPOINT",
		)
		region := firstProfessionalEnv(
			"CUSTOM_SKILLHUB_PROFESSIONAL_OBS_REGION",
			"OBS_REGION",
		)
		if region == "" {
			region = "cn-north-4"
		}
		accessKey := firstProfessionalEnv(
			"CUSTOM_SKILLHUB_PROFESSIONAL_OBS_ACCESS_KEY",
			"OBS_ACCESS_KEY",
		)
		secretKey := firstProfessionalEnv(
			"CUSTOM_SKILLHUB_PROFESSIONAL_OBS_SECRET_KEY",
			"OBS_SECRET_KEY",
		)
		if endpoint == "" || accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("incomplete professional skill OBS configuration")
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
		return nil, fmt.Errorf(
			"unsupported professional skill storage provider %q; only minio and obs are allowed",
			provider,
		)
	}
	if err != nil {
		return nil, err
	}
	privateService, ok := base.(interfaces.PrivateObjectFileService)
	if !ok {
		return nil, fmt.Errorf(
			"professional skill provider %q does not implement private object commits",
			provider,
		)
	}
	return &professionalObjectStore{
		service: privateService,
		uriRoot: provider + "://" + bucket + "/" + normalizedPrefix + "/",
	}, nil
}

func (s *professionalObjectStore) CheckConnectivity(ctx context.Context) error {
	if s == nil || s.service == nil {
		return fmt.Errorf("professional skill object storage is unavailable")
	}
	return s.service.CheckConnectivity(ctx)
}

func (s *professionalObjectStore) Reserve(
	tenantID uint64,
	skillID string,
	revisionID string,
) (string, error) {
	if s == nil || s.service == nil {
		return "", fmt.Errorf("professional skill object storage is unavailable")
	}
	if tenantID == 0 {
		return "", fmt.Errorf("professional skill tenant is required")
	}
	if err := plannedfile.ValidateSegment("skill ID", strings.TrimSpace(skillID)); err != nil {
		return "", err
	}
	if err := plannedfile.ValidateSegment("revision ID", strings.TrimSpace(revisionID)); err != nil {
		return "", err
	}
	return s.service.ReservePrivateObjectPath(
		"tenant",
		strconv.FormatUint(tenantID, 10),
		"skill",
		strings.TrimSpace(skillID),
		"revision",
		strings.TrimSpace(revisionID),
		"package.zip",
	)
}

func (s *professionalObjectStore) owns(filePath string) bool {
	return s != nil &&
		s.uriRoot != "" &&
		strings.HasPrefix(strings.TrimSpace(filePath), s.uriRoot)
}

func (s *professionalObjectStore) CommitAndVerify(
	ctx context.Context,
	data []byte,
	filePath string,
	contentType string,
	digest string,
) error {
	if !s.owns(filePath) {
		return fmt.Errorf("professional skill path is outside the private object namespace")
	}
	if err := s.service.CommitPrivateObjectAtPath(ctx, data, filePath, contentType, digest); err != nil {
		return err
	}
	return s.service.VerifyPrivateObject(ctx, filePath, int64(len(data)), digest)
}

func (s *professionalObjectStore) Verify(
	ctx context.Context,
	filePath string,
	size int64,
	digest string,
) error {
	if !s.owns(filePath) {
		return fmt.Errorf("professional skill path is outside the private object namespace")
	}
	return s.service.VerifyPrivateObject(ctx, filePath, size, digest)
}

func (s *professionalObjectStore) Open(ctx context.Context, filePath string) (io.ReadCloser, error) {
	if !s.owns(filePath) {
		return nil, fmt.Errorf("professional skill path is outside the private object namespace")
	}
	return s.service.GetFile(ctx, filePath)
}

func (s *professionalObjectStore) Delete(ctx context.Context, filePath string) error {
	if !s.owns(filePath) {
		return fmt.Errorf("professional skill path is outside the private object namespace")
	}
	return s.service.DeleteFile(ctx, filePath)
}

func professionalStorageProvider() string {
	return strings.ToLower(firstProfessionalEnv(
		"CUSTOM_SKILLHUB_PROFESSIONAL_STORAGE_PROVIDER",
	))
}

func firstProfessionalEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func professionalEnvBool(name string, fallback bool) bool {
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
