//go:build windows

package process

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcessControl struct {
	job windows.Handle
}

func startManaged(command *exec.Cmd) (processControl, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if err := command.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	processHandle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		_ = command.Process.Kill()
		windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(processHandle)
	if err := windows.AssignProcessToJobObject(job, processHandle); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
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
