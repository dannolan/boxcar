//go:build windows

package romfs

import "errors"

// mkFifo is unavailable in the standard library on Windows; returning an
// error makes TestBuildSkipsSpecialFiles skip on that platform.
func mkFifo(path string) error {
	return errors.New("named pipes require POSIX")
}
