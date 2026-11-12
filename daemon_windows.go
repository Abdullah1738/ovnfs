//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// lockStart waits for the state's start lock, serializing concurrent starts;
// closing the returned file releases it.
func lockStart(ctx context.Context, state string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(state, startLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	for {
		err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			break
		}
		if err != windows.ERROR_LOCK_VIOLATION {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	return f, nil
}

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

func daemonStopped(context.Context, int) bool { return false }

func signalDaemon(pid int, force bool) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func fastStartLease(state string) (*os.File, error) {
	f, err := os.Open(filepath.Join(state, startLock))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err != nil {
		f.Close()
		if err == windows.ERROR_LOCK_VIOLATION {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

// Process discovery in pendingStartupHolder preserves exclusion after the
// CLI process exits; a goroutine retaining a handle would not survive it.
func releaseStartupLease(lease *os.File, _ string, _ int, _ error) { lease.Close() }
