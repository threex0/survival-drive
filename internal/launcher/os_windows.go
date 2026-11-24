//go:build windows

package launcher

import (
	"syscall"
	"unsafe"
)

// exeName adds the platform executable suffix.
func exeName(base string) string { return base + ".exe" }

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	globalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// RAM reports installed and currently available physical memory in bytes.
//
// Available matters more than installed: kiwix-serve holds ~900 MB once it has
// indexed 25 archives, and a model that fits "in total RAM" can still push a
// small machine into swapping or into an out-of-memory kill that takes the
// library down with it.
func RAM() (total, available int64, err error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, callErr := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, 0, callErr
	}
	return int64(m.TotalPhys), int64(m.AvailPhys), nil
}

// TotalRAM reports installed physical memory in bytes.
func TotalRAM() (int64, error) {
	t, _, err := RAM()
	return t, err
}
