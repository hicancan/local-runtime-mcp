//go:build windows

package process

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestConfigureManagedCommandSetsCreateNoWindow(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "echo", "ok")
	configureManagedCommand(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr to be initialized")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("expected CREATE_NO_WINDOW flag, got 0x%x", cmd.SysProcAttr.CreationFlags)
	}
	if cmd.SysProcAttr.HideWindow {
		t.Fatal("HideWindow must remain false so explicit GUI processes are not forcibly hidden")
	}
}
