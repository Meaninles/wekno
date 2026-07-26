package artifactstore

import (
	"context"
	"io"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/stretchr/testify/require"
)

type reservationOnlyService struct {
	bucket string
	prefix string
}

func (s *reservationOnlyService) ReservePrivateObjectPath(segments ...string) (string, error) {
	key, err := plannedfile.BuildKey(s.prefix, segments...)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath("minio", s.bucket, key)
}

func (*reservationOnlyService) CheckConnectivity(context.Context) error { return nil }
func (*reservationOnlyService) SaveFile(context.Context, *multipart.FileHeader, uint64, string) (string, error) {
	panic("not used")
}
func (*reservationOnlyService) SaveBytes(context.Context, []byte, uint64, string, bool) (string, error) {
	panic("not used")
}
func (*reservationOnlyService) GetFile(context.Context, string) (io.ReadCloser, error) {
	panic("not used")
}
func (*reservationOnlyService) GetFileURL(context.Context, string) (string, error) {
	panic("not used")
}
func (*reservationOnlyService) DeleteFile(context.Context, string) error {
	panic("not used")
}
func (*reservationOnlyService) CopyFile(context.Context, string, uint64, string) (string, error) {
	panic("not used")
}
func (*reservationOnlyService) CommitPrivateObjectAtPath(context.Context, []byte, string, string, string) error {
	panic("not used")
}
func (*reservationOnlyService) VerifyPrivateObject(context.Context, string, int64, string) error {
	panic("not used")
}

func newReservationTestStore(t *testing.T) *Store {
	t.Helper()
	prefix := "weknora/__weknora_private_agent_artifacts_v1__/deployment/dev-local/namespace/74b3d025-5a14-4a6d-b0fc-ff228d0ba98c"
	return &Store{
		service:  &reservationOnlyService{bucket: "private-artifacts", prefix: prefix},
		provider: "minio",
		bucket:   "private-artifacts",
		prefix:   prefix,
		uriRoot:  "minio://private-artifacts/" + prefix + "/",
	}
}

func TestReserveUsesVersionedTenantScopedCollisionResistantNamespace(t *testing.T) {
	store := newReservationTestStore(t)

	first, err := store.Reserve(
		10001,
		"session-a",
		"run-a",
		"artifact-a",
		"制度报告.DOCX",
	)
	require.NoError(t, err)
	require.Equal(
		t,
		"minio://private-artifacts/weknora/__weknora_private_agent_artifacts_v1__/deployment/dev-local/namespace/74b3d025-5a14-4a6d-b0fc-ff228d0ba98c/tenant/10001/session/session-a/run/run-a/artifact/artifact-a/artifact-a.docx",
		first,
	)
	require.True(t, store.Owns(first))

	// The same artifact reservation is deterministic for retry idempotency.
	retry, err := store.Reserve(10001, "session-a", "run-a", "artifact-a", "制度报告.DOCX")
	require.NoError(t, err)
	require.Equal(t, first, retry)

	// Every ownership dimension changes the key. UUID artifact IDs then make
	// independent uploads collision resistant even within the same run.
	paths := []string{first}
	for _, tc := range []struct {
		tenant     uint64
		session    string
		run        string
		artifactID string
	}{
		{10002, "session-a", "run-a", "artifact-a"},
		{10001, "session-b", "run-a", "artifact-a"},
		{10001, "session-a", "run-b", "artifact-a"},
		{10001, "session-a", "run-a", "artifact-b"},
	} {
		got, reserveErr := store.Reserve(tc.tenant, tc.session, tc.run, tc.artifactID, "制度报告.DOCX")
		require.NoError(t, reserveErr)
		require.NotContains(t, paths, got)
		paths = append(paths, got)
	}
}

func TestReserveRejectsHierarchyInjectionAndOwnsRequiresExactRoot(t *testing.T) {
	store := newReservationTestStore(t)

	for _, tc := range []struct {
		session    string
		run        string
		artifactID string
	}{
		{"../session", "run-a", "artifact-a"},
		{"session-a", `run\escape`, "artifact-a"},
		{"session-a", "run-a", "artifact/escape"},
	} {
		_, err := store.Reserve(10001, tc.session, tc.run, tc.artifactID, "report.pdf")
		require.Error(t, err)
	}

	require.False(t, store.Owns(
		"minio://private-artifacts/weknora/__weknora_private_agent_artifacts_v1__/deployment/dev-local/namespace/74b3d025-5a14-4a6d-b0fc-ff228d0ba98cevil/object",
	))
	require.False(t, store.Owns(
		"minio://another-bucket/weknora/__weknora_private_agent_artifacts_v1__/deployment/dev-local/namespace/74b3d025-5a14-4a6d-b0fc-ff228d0ba98c/object",
	))
	require.True(t, strings.Contains(store.Prefix(), "/namespace/"))
}

func TestValidatePrivateNamespacePrefix(t *testing.T) {
	require.NoError(t, validatePrivateNamespacePrefix(
		"weknora/__weknora_private_agent_artifacts_v1__/deployment/prod-cce-wk-7b2f0a97/namespace/7b2f0a97-3a5c-47e6-9d14-c860f1e6a3b2",
	))

	for _, prefix := range []string{
		"weknora/__weknora_private_agent_artifacts_v1__",
		"weknora/__weknora_private_agent_artifacts_v1__/deployment/prod/namespace/not-a-uuid",
		"weknora/__weknora_private_agent_artifacts_v1__/deployment/Prod/namespace/7b2f0a97-3a5c-47e6-9d14-c860f1e6a3b2",
		"weknora/__weknora_private_agent_artifacts_v1__/deployment/prod/namespace/00000000-0000-0000-0000-000000000000",
		"weknora/__weknora_private_agent_artifacts_v1__/deployment/prod/namespace/7B2F0A97-3A5C-47E6-9D14-C860F1E6A3B2",
		"weknora/__weknora_private_agent_artifacts_v1__/deployment/customer/name/namespace/7b2f0a97-3a5c-47e6-9d14-c860f1e6a3b2",
	} {
		require.Error(t, validatePrivateNamespacePrefix(prefix), prefix)
	}
}
