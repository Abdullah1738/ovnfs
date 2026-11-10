//go:build windows

package main

import (
	"context"
	"github.com/stencil-hq/ovnfs/checkout"
	"golang.org/x/sys/windows"
	"unsafe"
)

func processOwnsState(ctx context.Context, pid int, state string) bool {
	owner, held, err := checkout.StateHolder(state)
	if err != nil || !held || owner != pid {
		return false
	}
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
