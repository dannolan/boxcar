//go:build freebsd || openbsd || netbsd || dragonfly

package nup

import "errors"

// copyXattrs: Boxcar cannot carry BSD extended attributes over, so in-place
// rewrites are refused there rather than silently dropping them (BSD is not
// a release target).
func copyXattrs(string, string) error {
	return errors.New("In-place rewrites are not supported on this system; write a new file with --output instead.")
}
