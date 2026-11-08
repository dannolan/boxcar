//go:build windows

package patch

import (
	"os"

	"golang.org/x/sys/windows"
)

// linked reports whether path is a symlink or reparse point (Windows).
func linked(path string) bool {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return true
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attr, err := windows.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return attr&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// flock acquires (exclusive, non-blocking) or releases a byte-range lock on f.
func flock(f *os.File, exclusive bool) error {
	var ol windows.Overlapped
	if exclusive {
		flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
		return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ol)
	}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
