package file

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCleanupStaleBoundCopyStagesOnlyRemovesOldRegularStageFiles(t *testing.T) {
	now := time.Now().Round(time.Second)
	maxAge := time.Hour

	stale, err := NewBoundCopyStage()
	require.NoError(t, err)
	stalePath := stale.Name()
	require.NoError(t, stale.Close())
	require.NoError(t, os.Chtimes(stalePath, now.Add(-2*maxAge), now.Add(-2*maxAge)))
	t.Cleanup(func() { _ = os.Remove(stalePath) })

	fresh, err := NewBoundCopyStage()
	require.NoError(t, err)
	freshPath := fresh.Name()
	require.NoError(t, fresh.Close())
	require.NoError(t, os.Chtimes(freshPath, now.Add(-maxAge/2), now.Add(-maxAge/2)))
	t.Cleanup(func() { _ = os.Remove(freshPath) })

	unrelated, err := os.CreateTemp(os.TempDir(), "not-a-weknora-stage-*")
	require.NoError(t, err)
	unrelatedPath := unrelated.Name()
	require.NoError(t, unrelated.Close())
	require.NoError(t, os.Chtimes(unrelatedPath, now.Add(-2*maxAge), now.Add(-2*maxAge)))
	t.Cleanup(func() { _ = os.Remove(unrelatedPath) })

	target, err := os.CreateTemp(os.TempDir(), "bound-copy-symlink-target-*")
	require.NoError(t, err)
	targetPath := target.Name()
	require.NoError(t, target.Close())
	t.Cleanup(func() { _ = os.Remove(targetPath) })
	symlinkPath := filepath.Join(os.TempDir(), boundCopyTempPrefix+"symlink-test")
	_ = os.Remove(symlinkPath)
	symlinkCreated := os.Symlink(targetPath, symlinkPath) == nil
	if symlinkCreated {
		t.Cleanup(func() { _ = os.Remove(symlinkPath) })
	}

	require.NoError(t, CleanupStaleBoundCopyStages(now, maxAge))
	_, err = os.Stat(stalePath)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(freshPath)
	require.NoError(t, err)
	_, err = os.Stat(unrelatedPath)
	require.NoError(t, err)
	_, err = os.Stat(targetPath)
	require.NoError(t, err)
	if symlinkCreated {
		info, err := os.Lstat(symlinkPath)
		require.NoError(t, err)
		require.NotZero(t, info.Mode()&os.ModeSymlink)
	}
}
