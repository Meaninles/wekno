//go:build windows

package file

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func localRootIdentity(baseDir string) (string, error) {
	absolute, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("canonicalize local storage root: %w", err)
	}
	path, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return "", fmt.Errorf("encode local storage root: %w", err)
	}
	handle, err := windows.CreateFile(
		path, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0,
	)
	if err != nil {
		return "", fmt.Errorf("open local storage root: %w", err)
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", fmt.Errorf("identify local storage root: %w", err)
	}
	return fmt.Sprintf("windows:%x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}
