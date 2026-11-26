//go:build windows

package cloudflare

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// configureCommand prevents cloudflared from allocating or flashing a console window.
func configureCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
