//go:build !windows

package file

import (
	"fmt"
	"os"
	"syscall"
)

func localRootIdentity(baseDir string) (string, error) {
	info, err := os.Stat(baseDir)
	if err != nil {
		return "", fmt.Errorf("stat local storage root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("local storage root is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return "", fmt.Errorf("local storage root identity is unavailable")
	}
	return fmt.Sprintf("unix:%x:%x", uint64(stat.Dev), uint64(stat.Ino)), nil
}
