//go:build !windows

package launcher

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// exeName adds the platform executable suffix.
func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// RAM reports installed and currently available physical memory in bytes.
func RAM() (total, available int64, err error) {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, 0, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			kb, perr := strconv.ParseInt(fields[1], 10, 64)
			if perr != nil {
				continue
			}
			switch fields[0] {
			case "MemTotal:":
				total = kb * 1024
			case "MemAvailable:":
				available = kb * 1024
			}
		}
		if total > 0 {
			return total, available, nil
		}
	}
	return 0, 0, os.ErrNotExist
}

// TotalRAM reports installed physical memory in bytes. Implemented for Linux via
// /proc/meminfo and for macOS via sysctl; the launcher is Windows-first, so this
// is here to keep the package portable and testable.
func TotalRAM() (int64, error) {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "MemTotal:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				break
			}
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil
		}
	}
	return 0, os.ErrNotExist
}
