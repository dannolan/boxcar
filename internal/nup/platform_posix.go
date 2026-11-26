//go:build !windows

package nup

import (
	"errors"
	"io/fs"
	"os"
)

// commitNew moves temporary onto output without overwriting an existing
// file, like commitFile. On filesystems without hard links (exFAT and FAT32
// SD cards) output is first claimed with O_EXCL and the claim is then
// atomically replaced by rename(2). Once output is linked the commit has
// happened; failing to drop the temporary name only leaves clutter.
func commitNew(temporary, output string) error {
	err := os.Link(temporary, output)
	switch {
	case err == nil:
		_ = os.Remove(temporary)
		return nil
	case errors.Is(err, fs.ErrExist):
		return err
	default:
		return claimAndRename(temporary, output)
	}
}

func claimAndRename(temporary, output string) error {
	claim, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	claim.Close()
	if err := os.Rename(temporary, output); err != nil {
		_ = os.Remove(output)
		return err
	}
	return nil
}

// replaceable reports why a file cannot be replaced in place; rename(2)
// only needs a writable directory.
func replaceable(os.FileInfo) error { return nil }

// replaceFile atomically swaps temporary over an existing output: rename(2)
// leaves either the old or the new file at output, never a partial one. The
// output's permissions are copied where the filesystem keeps them.
func replaceFile(temporary, output string, perm os.FileMode) error {
	if st, err := os.Stat(temporary); err == nil && st.Mode().Perm() != perm {
		_ = os.Chmod(temporary, perm)
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
