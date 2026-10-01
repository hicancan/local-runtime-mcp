//go:build !windows

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	pty "github.com/aymanbagabas/go-pty"
)

type unixProcessControl struct {
	process *os.Process
}

func preparePTY(*pty.Cmd) {}

func attachManaged(process *os.Process) (processControl, error) {
	return &unixProcessControl{process: process}, nil
}

func startManaged(command *exec.Cmd) (processControl, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &unixProcessControl{process: command.Process}, nil
}

func (c *unixProcessControl) Kill() error {
	err := syscall.Kill(-c.process.Pid, syscall.SIGKILL)
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (c *unixProcessControl) Close() error { return c.Kill() }

func finishTerminal(terminal pty.Pty, outputDone <-chan struct{}) {
	// Allow the master reader to collect the slave's final bytes before closing.
	select {
	case <-outputDone:
	case <-time.After(100 * time.Millisecond):
	}
	_ = terminal.Close()
	<-outputDone
}
