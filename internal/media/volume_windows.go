//go:build windows

package media

import (
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	getVolumeInformationW = kernel32.NewProc("GetVolumeInformationW")
	getDiskFreeSpaceExW   = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// VolumeInfo describes a mounted volume.
type VolumeInfo struct {
	Label      string
	FileSystem string
	Total      int64
	Free       int64
}

// Inspect reports label, filesystem and capacity for a drive root ("E:\").
func Inspect(root string) (VolumeInfo, error) {
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return VolumeInfo{}, err
	}

	var (
		labelBuf = make([]uint16, 256)
		fsBuf    = make([]uint16, 256)
		serial   uint32
		maxLen   uint32
		fsFlags  uint32
		info     VolumeInfo
	)
	r, _, callErr := getVolumeInformationW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&labelBuf[0])),
		uintptr(len(labelBuf)),
		uintptr(unsafe.Pointer(&serial)),
		uintptr(unsafe.Pointer(&maxLen)),
		uintptr(unsafe.Pointer(&fsFlags)),
		uintptr(unsafe.Pointer(&fsBuf[0])),
		uintptr(len(fsBuf)),
	)
	if r == 0 {
		return VolumeInfo{}, callErr
	}
	info.Label = syscall.UTF16ToString(labelBuf)
	info.FileSystem = syscall.UTF16ToString(fsBuf)

	var freeForCaller, total, totalFree uint64
	r, _, callErr = getDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeForCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return info, callErr
	}
	info.Free = int64(freeForCaller)
	info.Total = int64(total)
	return info, nil
}

// FreeBytes reports free space on the volume containing path.
func FreeBytes(path string) (int64, error) {
	info, err := Inspect(path)
	if err != nil {
		return 0, err
	}
	return info.Free, nil
}
