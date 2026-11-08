//go:build !windows

package dlc

import "os"

// commitFile commits the completed temp file to output without replacing an
// existing file. On POSIX this mirrors Python's os.link: an exclusive
// same-filesystem hard link; the caller removes the temp afterwards.
func commitFile(src, dst string) error {
	return os.Link(src, dst)
}
