//go:build windows

package proc

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepareCmd(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Hide console window if background worker
	cmd.SysProcAttr.HideWindow = true
	// CREATE_NEW_PROCESS_GROUP
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

func attachProcessSupervisor(pid int) func() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}

	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		windows.CloseHandle(job)
		return nil
	}

	hProcess, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil
	}
	defer windows.CloseHandle(hProcess)

	if err := windows.AssignProcessToJobObject(job, hProcess); err != nil {
		windows.CloseHandle(job)
		return nil
	}

	return func() {
		// Closing the handle terminates any residual child processes thanks to JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.CloseHandle(job)
	}
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}
