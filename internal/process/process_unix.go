//go:build !windows

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type unixProcessControl struct {
	process *os.Process
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

func (*unixProcessControl) Close() error { return nil }
