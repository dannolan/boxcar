package testutil

import (
	"os"
	"path/filepath"
	"strings"
)

// OpenFilesUnder lists files under dir that this process still holds open,
// including ones already unlinked, where the OS exposes its descriptors
// (/proc/self/fd). known is false where it does not; callers then rely on
// removability checks, which fail on Windows while a file is open.
func OpenFilesUnder(dir string) (open []string, known bool) {
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, false
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	prefix := dir + string(filepath.Separator)
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && strings.HasPrefix(target, prefix) {
			open = append(open, target)
		}
	}
	return open, true
}
