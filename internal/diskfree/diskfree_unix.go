//go:build !windows

package diskfree

import "syscall"

// Available returns free bytes for unprivileged users (st_bavail), matching
// shutil.disk_usage semantics.
func Available(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
