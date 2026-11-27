//go:build darwin

package nup

import "golang.org/x/sys/unix"

// renameNoReplace atomically moves temporary to output, failing with
// EEXIST when output exists (renamex_np RENAME_EXCL).
func renameNoReplace(temporary, output string) error {
	return unix.RenamexNp(temporary, output, unix.RENAME_EXCL)
}
