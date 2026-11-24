//go:build !windows

package media

import "errors"

// VolumeInfo describes a mounted volume.
type VolumeInfo struct {
	Label      string
	FileSystem string
	Total      int64
	Free       int64
}

// Inspect is only implemented on Windows; the tool targets Windows media.
func Inspect(root string) (VolumeInfo, error) {
	return VolumeInfo{}, errors.New("volume inspection is only implemented on Windows")
}

// FreeBytes reports free space on the volume containing path.
func FreeBytes(path string) (int64, error) {
	return 0, errors.New("free-space check is only implemented on Windows")
}
