//go:build linux || darwin

package nup

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// copyXattrs makes dst's extended attributes (POSIX ACLs, macOS ACLs and
// quarantine flags, SELinux labels) equal to src's. An attribute that
// cannot be set or removed fails the copy unless dst already matches.
// Filesystems without extended attributes have nothing to copy.
func copyXattrs(src, dst string) error {
	want, err := xattrs(src)
	if err != nil {
		return err
	}
	have, err := xattrs(dst)
	if err != nil {
		return err
	}
	for name, v := range want {
		if old, ok := have[name]; ok && bytes.Equal(old, v) {
			continue
		}
		if err := unix.Setxattr(dst, name, v, 0); err != nil {
			return fmt.Errorf("Cannot copy the original's extended attribute %q to the optimized NUP (%v); the original was kept.", name, err)
		}
	}
	for name := range have {
		if _, ok := want[name]; ok {
			continue
		}
		if err := unix.Removexattr(dst, name); err != nil {
			return fmt.Errorf("Cannot drop extended attribute %q, which the original does not have, from the optimized NUP (%v); the original was kept.", name, err)
		}
	}
	return nil
}

func unsupportedXattr(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}

func xattrs(path string) (map[string][]byte, error) {
	out := map[string][]byte{}
	size, err := unix.Listxattr(path, nil)
	if unsupportedXattr(err) || size == 0 {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	size, err = unix.Listxattr(path, buf)
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(string(buf[:size]), "\x00") {
		if name == "" {
			continue
		}
		n, err := unix.Getxattr(path, name, nil)
		if err != nil {
			return nil, err
		}
		v := make([]byte, n)
		if n > 0 {
			if n, err = unix.Getxattr(path, name, v); err != nil {
				return nil, err
			}
		}
		out[name] = v[:n]
	}
	return out, nil
}
