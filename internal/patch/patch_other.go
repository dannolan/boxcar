//go:build !windows

package patch

import (
	"os"
	"syscall"
)

// linked reports whether path is a symlink (POSIX; no reparse-point handling).
func linked(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0
}

// flock acquires (exclusive) or releases the lock on f. Non-blocking; returns
// an error if the lock is already held by another process.
func flock(f *os.File, exclusive bool) error {
	if exclusive {
		return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
