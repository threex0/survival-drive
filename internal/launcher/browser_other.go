//go:build !windows

package launcher

import (
	"os/exec"
	"runtime"
)

// openBrowser opens the default browser at url.
func openBrowser(url string) {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	_ = exec.Command(bin, url).Start()
}
