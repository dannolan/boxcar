//go:build !windows

package nuptools

import (
	"os"
	"syscall"
)

// fileIno returns the inode number of a file, matching Python st_ino.
func fileIno(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
