package knowledgeaux

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestSourceFileServiceForReadAllowsOnlyExactLegacyOwner(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	path := "local://7/knowledge-1/source.pdf"
	require.NoError(t, db.Model(owner).Update("file_path", path).Error)

	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})

	_, err := registry.FileServiceForPath(
		context.Background(), 7, "kb-1", "knowledge-1", path, "local",
	)
	require.ErrorIs(t, err, ErrReservationLost)

	service, err := registry.SourceFileServiceForRead(
		context.Background(), 7, "kb-1", "knowledge-1", path, "local",
	)
	require.NoError(t, err)
	require.Same(t, local, service)

	_, err = registry.SourceFileServiceForRead(
		context.Background(), 7, "wrong-kb", "knowledge-1", path, "local",
	)
	require.ErrorIs(t, err, ErrReservationLost)
	_, err = registry.SourceFileServiceForRead(
		context.Background(), 7, "kb-1", "knowledge-1", "local://7/other.pdf", "local",
	)
	require.ErrorIs(t, err, ErrReservationLost)
}

func TestSourceFileServiceForReadDoesNotBypassQuarantine(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	path := "local://7/knowledge-1/source.pdf"
	require.NoError(t, db.Model(owner).Update("file_path", path).Error)

	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor(path, "generation-1", KindSourceFile)
	object.FallbackProvider = "local"
	object.Quarantined = true
	object.QuarantineReason = quarantineReasonLegacyIdentity
	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)

	_, err = registry.SourceFileServiceForRead(
		context.Background(), 7, "kb-1", "knowledge-1", path, "local",
	)
	require.ErrorIs(t, err, ErrBindingQuarantined)
}
