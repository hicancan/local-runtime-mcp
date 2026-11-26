//go:build windows

package process

import (
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	pty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/windows"
)

type windowsProcessControl struct {
	job windows.Handle
}

func configureManagedCommand(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	// CREATE_NO_WINDOW prevents Windows (and Windows 11 Windows Terminal) from
	// allocating a flashing conhost/terminal window when the headless daemon
	// spawns CLI tools like powershell.exe, pwsh.exe, git.exe, or python.exe,
	// while still allowing intentional GUI windows (e.g. WinForms/apps) to show.
	command.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

func startManaged(command *exec.Cmd) (processControl, error) {
	configureManagedCommand(command)
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

func finishTerminal(terminal pty.Pty, outputDone <-chan struct{}) {
	console, ok := terminal.(pty.ConPty)
	if !ok {
		_ = terminal.Close()
		<-outputDone
		return
	}
	// Keep the reader open while ConHost publishes its final output. The upstream
	// Close closes the read pipe immediately after ClosePseudoConsole and can lose
	// the last frame from a short-lived executable.
	windows.ClosePseudoConsole(windows.Handle(console.Fd()))
	_ = console.InputPipe().Close()
	select {
	case <-outputDone:
	case <-time.After(2 * time.Second):
	}
	_ = console.OutputPipe().Close()
	<-outputDone
}
