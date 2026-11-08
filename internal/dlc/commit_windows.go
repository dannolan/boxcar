//go:build windows

package dlc

import "golang.org/x/sys/windows"

// commitFile commits the completed temp file to output without replacing an
// existing file. On Windows this mirrors Python's temporary.rename(output):
// MoveFileEx without MOVEFILE_REPLACE_EXISTING, so it refuses to overwrite
// (Go's os.Rename would replace). MOVEFILE_WRITE_THROUGH is used so the rename
// is flushed to disk like a rename-through-flush.
func commitFile(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}
