//go:build windows

package launcher

import (
	"os/exec"
	"syscall"
)

// openBrowser opens the default browser at url.
func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}
