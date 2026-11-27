//go:build linux

package nup

import "golang.org/x/sys/unix"

func physicalMemory() int64 {
	var si unix.Sysinfo_t
	if unix.Sysinfo(&si) != nil {
		return 0
	}
	return int64(si.Totalram) * int64(si.Unit)
}
