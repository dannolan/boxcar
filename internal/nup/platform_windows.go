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

// Partial-failure codes of ReplaceFileW (winerror.h).
const (
	errUnableToRemoveReplaced   = syscall.Errno(1175)
	errUnableToMoveReplacement  = syscall.Errno(1176)
	errUnableToMoveReplacement2 = syscall.Errno(1177)
)

// Hooks for tests that simulate ReplaceFileW's documented partial failures
// and a failing recovery.
var (
	replaceFileW = func(replaced, replacement, backup string) error {
		to, err := windows.UTF16PtrFromString(replaced)
		if err != nil {
			return err
		}
		from, err := windows.UTF16PtrFromString(replacement)
		if err != nil {
			return err
		}
		bak, err := windows.UTF16PtrFromString(backup)
		if err != nil {
			return err
		}
		r, _, e := procReplaceFileW.Call(uintptr(unsafe.Pointer(to)), uintptr(unsafe.Pointer(from)),
			uintptr(unsafe.Pointer(bak)), 0, 0, 0)
		if r == 0 {
			return e
		}
		return nil
	}
	restoreBackup = commitNew // (MoveFileEx without replace: the path is empty when it runs)
)

// replaceFile swaps temporary over output with ReplaceFileW, which keeps the
// replaced file's ACLs, attributes, creation time and streams (merge errors
// are not ignored). ReplaceFileW is not atomic and its failures are not all
// clean: with ERROR_UNABLE_TO_MOVE_REPLACEMENT_2 (1177) the original has
// already been moved away and nothing is at output. So the original is
// always moved to a backup name in the same folder, and whatever the call
// returns, the resulting state is inspected (by file identity) before
// anything is reported or deleted:
//
//   - output holds the new file: success; the backup is deleted only then;
//   - output still holds the original (1175, 1176 and other failures):
//     failure, original kept;
//   - output is empty and the backup is the original (1177): the backup is
//     moved back and the run fails with the original kept; if that move
//     fails too, both the backup and the optimized copy are kept and the
//     error names them (keepFilesError);
//   - anything else: failure, every file kept.
//
// The only recoverable copy of the original is never deleted.
func replaceFile(temporary, output string) error {
	if err := flushFile(temporary); err != nil {
		return err
	}
	orig, err := os.Stat(output)
	if err != nil {
		return err
	}
	repl, err := os.Stat(temporary)
	if err != nil {
		return err
	}
	backup := output + "." + randomHex() + ".boxcar-backup"
	callErr := replaceFileW(output, temporary, backup)
	return settleReplace(output, temporary, backup, orig, repl, callErr)
}

func settleReplace(output, temporary, backup string, orig, repl os.FileInfo, callErr error) error {
	stat := func(p string) os.FileInfo {
		if st, err := os.Stat(p); err == nil {
			return st
		}
		return nil
	}
	out, tmp, bak := stat(output), stat(temporary), stat(backup)
	isOrig := func(st os.FileInfo) bool { return st != nil && os.SameFile(st, orig) }
	// The replacement normally keeps its own file ID; its size identifies
	// it too, and it must have left its temporary name.
	isNew := out != nil && !isOrig(out) && tmp == nil &&
		(os.SameFile(out, repl) || out.Size() == repl.Size())
	cause := "ReplaceFileW reported no error"
	if callErr != nil {
		cause = callErr.Error()
	}
	switch {
	case isNew:
		if bak != nil && isOrig(bak) {
			_ = os.Remove(backup) // the original is no longer needed; a leftover is harmless
		}
		return nil
	case isOrig(out):
		if bak != nil && !isOrig(bak) {
			_ = os.Remove(backup) // (not the original: nothing of value)
		}
		return fmt.Errorf("Cannot replace the NUP (%s); the original was kept.", cause)
	case out == nil && isOrig(bak):
		if rerr := restoreBackup(backup, output); rerr == nil && isOrig(stat(output)) {
			return fmt.Errorf("Cannot replace the NUP (%s); the original was moved back and kept.", cause)
		} else if rerr == nil {
			rerr = errors.New("the restored file is not the original")
			return &keepFilesError{fmt.Sprintf("Cannot replace the NUP (%s), and restoring it failed (%v). "+
				"The original is kept as %s and the verified optimized copy as %s; rename one of them to %s.",
				cause, rerr, backup, temporary, output)}
		} else {
			return &keepFilesError{fmt.Sprintf("Cannot replace the NUP (%s), and restoring it failed (%v). "+
				"The original is kept as %s and the verified optimized copy as %s; rename one of them to %s.",
				cause, rerr, backup, temporary, output)}
		}
	}
	where := ""
	if bak != nil {
		where = " A copy is kept as " + backup + "."
	}
	return &keepFilesError{fmt.Sprintf("Cannot replace the NUP (%s) and the files are in an unexpected state; nothing was deleted.%s "+
		"The verified optimized copy is %s.", cause, where, temporary)}
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
