//go:build windows

package connection

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
}
