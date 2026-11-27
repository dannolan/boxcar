package diskfree

import "golang.org/x/sys/unix"

// IsFAT reports whether path is on a FAT (msdos) volume, where no file can
// reach 4 GiB. exFAT has no such limit and is not FAT here.
func IsFAT(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, err
	}
	return unix.ByteSliceToString(st.Fstypename[:]) == "msdos", nil
}
