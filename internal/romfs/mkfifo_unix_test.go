//go:build !windows

package romfs

import "syscall"

// mkFifo creates a named pipe (POSIX) for the special-file skip test.
func mkFifo(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
