//go:build !darwin

package snapshot

import "os"

// mtimeNS returns the file modification time in nanoseconds since the Unix
// epoch. On non-Darwin platforms os.FileInfo.ModTime already carries full
// nanosecond precision (Linux and Windows), matching st_mtime_ns.
func mtimeNS(info os.FileInfo) int64 {
	return info.ModTime().UnixNano()
}
