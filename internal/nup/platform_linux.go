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

// replaceable reports why a file cannot be replaced in place; rename(2)
// only needs a writable directory.
func replaceable(os.FileInfo) error { return nil }

// replaceFile atomically swaps temporary over an existing output, giving it
// the output's permissions: rename(2) leaves either the old or the new file
// at output, never a partial one.
func replaceFile(temporary, output string, perm os.FileMode) error {
	if err := os.Chmod(temporary, perm); err != nil {
		return err
	}
	return os.Rename(temporary, output)
}

// syncDir flushes a directory entry change (best effort).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// isLinked reports whether path is a symlink (Python linked() on POSIX).
func isLinked(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0
}
