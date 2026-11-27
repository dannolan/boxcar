//go:build windows

package nup

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

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

// commitNew moves temporary onto output without replacing an existing file
// (MoveFileEx without MOVEFILE_REPLACE_EXISTING — an exclusive commit that
// needs no hard links, so it also works on exFAT and FAT32 volumes).
func commitNew(temporary, output string) error {
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

// lockSource coordinates in-place rewrites of path between Boxcar runs with
// a sidecar lock file opened without sharing and deleted on close: a second
// run gets a sharing violation, and the file disappears with the handle,
// also when the process dies. Failing to create it refuses the rewrite.
func lockSource(path string) (func(), error) {
	name, err := windows.UTF16PtrFromString(path + ".boxcar-lock")
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE, 0, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	switch {
	case err == nil:
		return func() { windows.CloseHandle(h) }, nil
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION), errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return nil, errors.New("Another process is optimizing this NUP.")
	default:
		return nil, fmt.Errorf("Cannot lock the NUP for an in-place rewrite (%v); write a new file with --output instead.", err)
	}
}

// replaceable reports why a file cannot be replaced in place: ReplaceFile
// refuses to replace a read-only file.
func replaceable(st os.FileInfo) error {
	if st.Mode().Perm()&0o200 == 0 {
		return errors.New("The NUP is read-only; make it writable to optimize it in place.")
	}
	return nil
}

// prepareReplacement has nothing to do on Windows: ReplaceFile carries the
// original's security descriptor, attributes and alternate streams over.
func prepareReplacement(string, string, os.FileInfo) error { return nil }

var procReplaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

// replaceFile swaps temporary over output with ReplaceFileW, which keeps the
// replaced file's ACLs, attributes, creation time and streams (merge errors
// are not ignored: a security descriptor that cannot be kept fails the
// call). Without a backup name output is either the old file or the new
// one; the replacement is flushed before the call returns.
func replaceFile(temporary, output string) error {
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(output)
	if err != nil {
		return err
	}
	if err := flushFile(temporary); err != nil {
		return err
	}
	r, _, e := procReplaceFileW.Call(uintptr(unsafe.Pointer(to)), uintptr(unsafe.Pointer(from)), 0, 0, 0, 0)
	if r == 0 {
		return fmt.Errorf("Cannot replace the NUP (%v); the original was kept.", e)
	}
	return nil
}

func flushFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// syncDir is a no-op: MOVEFILE_WRITE_THROUGH already flushed the rename.
func syncDir(string) {}

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
