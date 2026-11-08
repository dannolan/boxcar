//go:build windows

package nsp

import "golang.org/x/sys/windows"

// commitFile atomically moves temp to dst without overwriting an existing
// destination. Windows os.Rename would replace the target, so MoveFileEx is
// called without MOVEFILE_REPLACE_EXISTING to match Python's refusing rename.
func commitFile(temp, dst string) error {
	s, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	d, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(s, d, 0)
}

// renameCompanion also uses an exclusive MoveFileEx so an existing companion
// triggers the content re-check exactly like Python's os.rename on Windows.
func renameCompanion(temp, dst string) error {
	return commitFile(temp, dst)
}
