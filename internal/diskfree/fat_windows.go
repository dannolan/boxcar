package diskfree

import (
	"strings"

	"golang.org/x/sys/windows"
)

// IsFAT reports whether path is on a FAT or FAT32 volume, where no file can
// reach 4 GiB. exFAT has no such limit and is not FAT here.
func IsFAT(path string) (bool, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	root := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(p, &root[0], uint32(len(root))); err != nil {
		return false, err
	}
	name := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(&root[0], nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err != nil {
		return false, err
	}
	fs := strings.ToUpper(windows.UTF16ToString(name))
	return fs == "FAT" || fs == "FAT32", nil
}
