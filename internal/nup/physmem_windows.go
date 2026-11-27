//go:build windows

package nup

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func physicalMemory() int64 {
	var st struct {
		length, memoryLoad                  uint32
		totalPhys, availPhys                uint64
		totalPageFile, availPageFile        uint64
		totalVirtual, availVirtual, extVirt uint64
	}
	st.length = uint32(unsafe.Sizeof(st))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st))); r == 0 {
		return 0
	}
	return int64(st.totalPhys)
}
