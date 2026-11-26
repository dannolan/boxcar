//go:build linux

package nup

import (
	"os"
	"syscall"
)

// lstatSource captures the size/mtime/ctime/inode of path (or a symlink
// detection flag) without following links — the port of Python's
// path.lstat() + st_mtime_ns/st_ctime_ns/st_ino. Linux names the raw
// timespecs Mtim/Ctim.
func lstatSource(path string) (size, mtime, ctime, ino int64, linked bool, err error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return 0, 0, 0, 0, false, err
	}
	linked = st.Mode&syscall.S_IFMT == syscall.S_IFLNK
	size = st.Size
	mtime = st.Mtim.Sec*1e9 + st.Mtim.Nsec
	ctime = st.Ctim.Sec*1e9 + st.Ctim.Nsec
	ino = int64(st.Ino)
	return size, mtime, ctime, ino, linked, nil
}

// commitFile moves temporary onto output without overwriting (port of
// nup_format.commit_file's POSIX branch: os.link + unlink).
func commitFile(temporary, output string) error {
	if err := os.Link(temporary, output); err != nil {
		return err
	}
	return os.Remove(temporary)
}

// isLinked reports whether path is a symlink (Python linked() on POSIX).
func isLinked(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0
}
