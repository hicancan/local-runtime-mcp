//go:build windows

package process

import (
	"os"
	"os/exec"
	"unsafe"

	pty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/windows"
)

type windowsProcessControl struct {
	job windows.Handle
}

func startManaged(command *exec.Cmd) (processControl, error) {
	job, err := newWindowsJob()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return assignWindowsJob(job, command.Process)
}

func preparePTY(*pty.Cmd) {}

func attachManaged(process *os.Process) (processControl, error) {
	job, err := newWindowsJob()
	if err != nil {
		return nil, err
	}
	return assignWindowsJob(job, process)
}

func newWindowsJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func assignWindowsJob(job windows.Handle, process *os.Process) (processControl, error) {
	processHandle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		_ = process.Kill()
		windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(processHandle)
	if err := windows.AssignProcessToJobObject(job, processHandle); err != nil {
		_ = process.Kill()
		windows.CloseHandle(job)
		return nil, err
	}
	return &windowsProcessControl{job: job}, nil
}

func (c *windowsProcessControl) Kill() error {
	return windows.TerminateJobObject(c.job, 1)
}

func (c *windowsProcessControl) Close() error {
	return windows.CloseHandle(c.job)
}
