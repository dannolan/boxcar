//go:build windows

package diskfree

import "golang.org/x/sys/windows"

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
