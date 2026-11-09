//go:build windows

package main

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// detachedProcess starts the daemon on a console of its own that has no
// window, in its own process group and, when the invoking job allows it,
// outside that job: an OpenSSH session kills its job's processes at logout. A
// job forbidding breakaway keeps the daemon, which then ends with that job.
// DETACHED_PROCESS would leave the daemon with no console at all, and Windows
// then opens a visible one for every console program it runs (git, net, ssh);
// the hidden console is inherited by those children instead.
func detachedProcess() *syscall.SysProcAttr {
	flags := uint32(windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP)
	if mayLeaveJob() {
		flags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	return &syscall.SysProcAttr{CreationFlags: flags}
}

// processAlive reports whether pid names a process that has not exited.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	event, err := windows.WaitForSingleObject(h, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}

// mayLeaveJob reports whether this process runs in a job that lets children
// break away; outside any job the query fails and there is nothing to leave.
func mayLeaveJob() bool {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return false
	}
	return info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK != 0
}
