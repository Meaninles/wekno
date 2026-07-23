package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const boundCopyTempPrefix = "weknora-bound-copy-"

func NewBoundCopyStage() (*os.File, error) {
	return os.CreateTemp(os.TempDir(), boundCopyTempPrefix+"*")
}

// CleanupStaleBoundCopyStages removes only old regular files created by
// NewBoundCopyStage. Symlinks, directories, fresh files and unrelated names
// are never followed or removed.
func CleanupStaleBoundCopyStages(now time.Time, maxAge time.Duration) error {
	if maxAge <= 0 {
		return fmt.Errorf("cleanup bound-copy stages: invalid maximum age")
	}
	tempDir := filepath.Clean(os.TempDir())
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return fmt.Errorf("cleanup bound-copy stages: list temp directory: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, boundCopyTempPrefix) || filepath.Base(name) != name {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < maxAge {
			if err != nil {
				errs = append(errs, fmt.Errorf("inspect staged file %q: %w", name, err))
			}
			continue
		}
		candidate := filepath.Join(tempDir, name)
		relative, err := filepath.Rel(tempDir, candidate)
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			errs = append(errs, fmt.Errorf("reject staged file path %q", candidate))
			continue
		}
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove stale staged file %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

type plannedReaderCommitter interface {
	commitReaderAtPath(ctx context.Context, reader io.ReadSeeker, size int64, contentType, filePath string) error
}

// CommitReaderAtPath streams a source object into an already-reserved exact
// destination. It is used for copies across different storage bindings and
// intentionally has no []byte fallback for production providers.
func CommitReaderAtPath(
	ctx context.Context,
	service interfaces.PlannedFileService,
	reader io.ReadSeeker,
	size int64,
	contentType string,
	filePath string,
) error {
	if service == nil || reader == nil || size < 0 {
		return fmt.Errorf("planned stream commit: invalid input")
	}
	committer, ok := service.(plannedReaderCommitter)
	if !ok || committer == nil {
		return fmt.Errorf("planned stream commit: provider does not support bounded streaming")
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("planned stream commit: rewind staged source: %w", err)
	}
	return committer.commitReaderAtPath(ctx, reader, size, contentType, filePath)
}
