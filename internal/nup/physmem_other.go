//go:build !linux && !darwin && !windows

package nup

func physicalMemory() int64 { return 0 }
