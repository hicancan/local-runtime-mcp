//go:build windows

package cloudflare

import (
	"os/exec"
	"syscall"
)

// configureCommand prevents the companion from opening another console window.
func configureCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
