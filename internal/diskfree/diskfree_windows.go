//go:build windows

package diskfree

import (
	"strings"

	"golang.org/x/sys/windows"
)

// Available returns free bytes for unprivileged users via GetDiskFreeSpaceExW.
func Available(path string) (uint64, error) {
	var free, total, totalFree uint64
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}

// volumeKey names the volume holding path (its volume path name, so mount
// points inside a drive count as their own volume).
func volumeKey(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return strings.ToLower(windows.UTF16ToString(buf)), nil
}
