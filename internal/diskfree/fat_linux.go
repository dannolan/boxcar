package diskfree

import "golang.org/x/sys/unix"

// IsFAT reports whether path is on a FAT (FAT12/16/32, "vfat") volume, where
// no file can reach 4 GiB. exFAT has no such limit and is not FAT here.
// FUSE mounts report their own type and are not recognized.
func IsFAT(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, err
	}
	return st.Type == unix.MSDOS_SUPER_MAGIC, nil
}
