//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// lockStart waits for the state's start lock, serializing concurrent starts;
// closing the returned file releases it.
func lockStart(state string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(state, startLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	// Blocking: no LOCKFILE_FAIL_IMMEDIATELY.
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// detachedProcess starts the daemon without a console, in its own process group
// and, when the invoking job allows it, outside that job: an OpenSSH session
// kills its job's processes at logout. A job forbidding breakaway keeps the
// daemon, which then ends with that job.
func detachedProcess() *syscall.SysProcAttr {
	flags := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	if mayLeaveJob() {
		flags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	return &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
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
