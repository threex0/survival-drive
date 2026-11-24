//go:build !windows

package fetch

import "errors"

// FreeBytes is only implemented on Windows; elsewhere the space preflight is
// skipped rather than guessed (the tool is Windows-only by design).
func FreeBytes(path string) (int64, error) {
	return 0, errors.New("free-space check is only implemented on Windows")
}
