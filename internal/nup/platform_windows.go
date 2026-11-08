//go:build windows

package nup

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// lstatSource captures size/mtime (Windows has no portable ctime/ino) and
// detects symlinks and reparse points.
func lstatSource(path string) (size, mtime, ctime, ino int64, linked bool, err error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, 0, 0, 0, false, err
	}
	linked = fi.Mode()&os.ModeSymlink != 0
	if sys, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		linked = linked || sys.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return fi.Size(), fi.ModTime().UnixNano(), 0, 0, linked, nil
}

// commitFile moves temporary onto output without replacing an existing file
// (MoveFileEx without MOVEFILE_REPLACE_EXISTING — an exclusive commit).
func commitFile(temporary, output string) error {
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(output)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, 0)
}

// isLinked reports whether path is a symlink or reparse point.
func isLinked(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	linked := fi.Mode()&os.ModeSymlink != 0
	if sys, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		linked = linked || sys.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return linked
}
