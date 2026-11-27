//go:build linux

package nup

import "golang.org/x/sys/unix"

// renameNoReplace atomically moves temporary to output, failing with
// EEXIST when output exists (renameat2 RENAME_NOREPLACE; the exFAT and
// FAT drivers support it).
func renameNoReplace(temporary, output string) error {
	return unix.Renameat2(unix.AT_FDCWD, temporary, unix.AT_FDCWD, output, unix.RENAME_NOREPLACE)
}
