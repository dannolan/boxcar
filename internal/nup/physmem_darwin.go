//go:build darwin

package nup

import "golang.org/x/sys/unix"

func physicalMemory() int64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(n)
}
