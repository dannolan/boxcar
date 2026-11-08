//go:build darwin

package snapshot

import (
	"os"
	"syscall"
)

// mtimeNS returns the file modification time in nanoseconds since the Unix
// epoch, matching Python's os.stat_result.st_mtime_ns. On Darwin the raw
// Mtimespec is read from syscall.Stat_t for identical precision.
func mtimeNS(info os.FileInfo) int64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime().UnixNano()
	}
	return st.Mtimespec.Sec*1e9 + st.Mtimespec.Nsec
}
