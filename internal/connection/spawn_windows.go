//go:build windows

package connection

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureDetached(cmd *exec.Cmd) {
	// Per Win32 CreateProcessW, CREATE_NO_WINDOW is ignored when combined with
	// DETACHED_PROCESS; using CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW keeps
	// the detached daemon completely windowless without console flashes.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
}
