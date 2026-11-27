//go:build !windows

package nup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// errNoExclusivePublish reports a filesystem on which a new file cannot be
// published without the risk of replacing one that appeared meanwhile.
var errNoExclusivePublish = errors.New("This filesystem has neither hard links nor an exclusive rename, so the output cannot be published without risking an overwrite; write it to another drive and copy it over.")

// Indirections for tests that simulate exFAT/FAT32 behaviour.
var (
	linkFile        = os.Link
	exclusiveRename = renameNoReplace
)

// commitNew moves the complete, synced temporary file onto output without
// ever replacing an existing file and without output ever naming an empty
// or partial file: a hard link (link + unlink, as the reference does), else (exFAT and FAT32 SD
// cards) an exclusive rename. Where neither exists it fails and leaves
// output untouched. Once output is linked the commit has happened; failing
// to drop the temporary name only leaves clutter.
func commitNew(temporary, output string) error {
	err := linkFile(temporary, output)
	switch {
	case err == nil:
		_ = os.Remove(temporary)
		return nil
	case errors.Is(err, fs.ErrExist):
		return err
	}
	err = exclusiveRename(temporary, output)
	if errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.ENOTSUP) {
		return errNoExclusivePublish
	}
	return err
}

// lockSource holds an exclusive advisory flock on path for an in-place
// rewrite so two Boxcar runs cannot replace the same archive at once. The
// lock coordinates Boxcar runs, not other programs. A filesystem that
// cannot lock is refused rather than used unlocked.
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
		return nil, fmt.Errorf("This filesystem cannot lock the NUP for an in-place rewrite (%v); write a new file with --output instead.", err)
	}
}

// replaceable reports why a file cannot be replaced in place; rename(2)
// only needs a writable directory.
func replaceable(os.FileInfo) error { return nil }

// prepareReplacement gives temporary the source's permission bits, owner
// and extended attributes (POSIX ACLs included) before it is renamed over
// the source. Anything that cannot be carried over fails the rewrite, so
// the original is kept, unless the temporary file already has it (exFAT
// and FAT32 have one owner and mode for the whole volume).
func prepareReplacement(temporary, source string, orig os.FileInfo) error {
	if ss, ok := orig.Sys().(*syscall.Stat_t); ok {
		if err := keepOwner(temporary, int(ss.Uid), int(ss.Gid)); err != nil {
			return err
		}
	}
	want := orig.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	cerr := os.Chmod(temporary, want)
	st, err := os.Stat(temporary)
	if err != nil {
		return err
	}
	if st.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != want {
		if cerr == nil {
			cerr = fmt.Errorf("mode is %v", st.Mode().Perm())
		}
		return fmt.Errorf("Cannot give the optimized NUP the original's permissions (%v); the original was kept.", cerr)
	}
	return copyXattrs(source, temporary)
}

func keepOwner(temporary string, uid, gid int) error {
	st, err := os.Stat(temporary)
	if err != nil {
		return err
	}
	ts, ok := st.Sys().(*syscall.Stat_t)
	if !ok || (int(ts.Uid) == uid && int(ts.Gid) == gid) {
		return nil
	}
	if err := os.Chown(temporary, uid, gid); err != nil {
		return fmt.Errorf("Cannot give the optimized NUP the original's owner (%v); the original was kept.", err)
	}
	return nil
}

// replaceFile atomically swaps temporary over an existing output: rename(2)
// leaves either the old or the new file at output, never a partial one.
func replaceFile(temporary, output string) error {
	return os.Rename(temporary, output)
}

// syncDir flushes a directory entry change (best effort).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
