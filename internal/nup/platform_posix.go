//go:build !windows

package nup

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// commitNew moves temporary onto output without overwriting an existing
// file, like commitFile. Filesystems without hard links (exFAT and FAT32 SD
// cards) get an exclusive rename instead, and only where that is not
// available either is output claimed with O_EXCL and the claim replaced by
// rename(2). Once output is linked the commit has happened; failing to drop
// the temporary name only leaves clutter.
func commitNew(temporary, output string) error {
	err := os.Link(temporary, output)
	switch {
	case err == nil:
		_ = os.Remove(temporary)
		return nil
	case errors.Is(err, fs.ErrExist):
		return err
	}
	err = renameNoReplace(temporary, output)
	if errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EINVAL) {
		return claimAndRename(temporary, output)
	}
	return err
}

// claimAndRename is the last-resort exclusive commit: it creates output
// with O_EXCL and renames temporary over that claim, giving up (and
// removing the claim) if the claim was replaced in between. The check and
// the rename are not one atomic step: a process that writes into the empty
// claim in that instant loses its data. That needs a non-cooperating writer
// on a filesystem with neither hard links nor an exclusive rename, which is
// why this path is the last resort (documented in docs/OPTIMIZE.md).
func claimAndRename(temporary, output string) error {
	claim, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	claimed, err := claim.Stat()
	claim.Close()
	if err != nil {
		return err
	}
	ours := func() bool {
		st, err := os.Lstat(output)
		return err == nil && st.Size() == 0 && os.SameFile(st, claimed)
	}
	if !ours() {
		return errors.New("Output appeared while it was being written; choose a new filename.")
	}
	if err := os.Rename(temporary, output); err != nil {
		if ours() {
			_ = os.Remove(output)
		}
		return err
	}
	return nil
}

// lockSource holds an advisory lock on path for an in-place rewrite so two
// optimizers cannot replace the same archive at once. The lock is advisory:
// it coordinates Boxcar runs, not other programs. Filesystems that refuse
// flock (rare on local disks; some network mounts) are used unlocked, and
// replaceSource's identity check is then the only guard.
func lockSource(path string) (func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		return func() { f.Close() }, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		f.Close()
		return nil, errors.New("Another process is optimizing this NUP.")
	default:
		f.Close()
		return func() {}, nil
	}
}

// replaceable reports why a file cannot be replaced in place; rename(2)
// only needs a writable directory.
func replaceable(os.FileInfo) error { return nil }

// replaceFile atomically swaps temporary over an existing output: rename(2)
// leaves either the old or the new file at output, never a partial one. The
// output's permissions are copied where the filesystem keeps them; the
// temporary file was created with them, so a failed chmod can only leave it
// narrower.
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
