//go:build !windows

package nsp

import "os"

// commitFile atomically moves temp to dst without overwriting an existing
// destination. On POSIX this is done with a hard link + unlink, which fails
// with EEXIST when dst already exists (Python commit_file semantics).
func commitFile(temp, dst string) error {
	if err := os.Link(temp, dst); err != nil {
		return err
	}
	return os.Remove(temp)
}

// renameCompanion renames the companion temp file into place. On POSIX
// os.rename silently replaces an existing destination, matching Python's
// Path.rename.
func renameCompanion(temp, dst string) error {
	return os.Rename(temp, dst)
}
