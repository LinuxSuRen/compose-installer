//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// diskAvailable returns the available bytes of the disk holding dir
// (Windows: GetDiskFreeSpaceExW).
func diskAvailable(dir string) (uint64, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")
	var free, total, freeTotal uint64
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	r1, _, e := proc.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&freeTotal)),
	)
	if r1 == 0 {
		return 0, e
	}
	return free, nil
}
