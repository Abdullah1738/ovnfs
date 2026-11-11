//go:build windows

package main

import (
	"context"
	"github.com/stencil-hq/ovnfs/checkout"
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

func processOwnsState(ctx context.Context, pid int, state string) bool {
	owner, held, err := checkout.StateHolder(state)
	if err != nil || !held || owner != pid {
		return false
	}
	return processServesState(pid, state)
}

func processServesState(pid int, state string) bool {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(p)
	var data [131072]byte
	var size uint32
	if err := windows.NtQueryInformationProcess(p, 60, unsafe.Pointer(&data[0]), uint32(len(data)), &size); err != nil {
		return false
	}
	line := (*windows.NTUnicodeString)(unsafe.Pointer(&data[0]))
	args, err := windows.DecomposeCommandLine(line.String())
	return err == nil && servesState(args, state)
}

func legacyStateHolder(context.Context, string) int { return 0 }

// Windows cannot inherit its parent's start range lock. Observe a serve's
// exact command line even before it takes state.lock, so the parent exiting
// after a timeout cannot reopen the spawning window.
func pendingStartupHolder(ctx context.Context, state string) (int, bool, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, false, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if ctx.Err() != nil {
			return 0, false, ctx.Err()
		}
		pid := int(entry.ProcessID)
		if pid != os.Getpid() && processServesState(pid, state) {
			return pid, true, nil
		}
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return 0, false, err
	}
	return 0, false, nil
}
