//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// flock belongs to the open file description, so the child's duplicate
// preserves exclusion even if its waiting parent is canceled or exits.
func inheritStart(cmd *exec.Cmd, lease *os.File) int {
	cmd.ExtraFiles = append(cmd.ExtraFiles, lease)
	return 2 + len(cmd.ExtraFiles)
}

func inheritedStartLease(fd int, state string) (*os.File, error) {
	if fd == 0 {
		return nil, nil
	}
	if fd < 3 {
		return nil, fmt.Errorf("invalid startup descriptor %d", fd)
	}
	f := os.NewFile(uintptr(fd), "start.lock")
	if f == nil {
		return nil, fmt.Errorf("missing startup descriptor %d", fd)
	}
	syscall.CloseOnExec(fd)
	got, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	want, err := os.Stat(filepath.Join(state, startLock))
	if err != nil || !os.SameFile(got, want) {
		f.Close()
		return nil, fmt.Errorf("startup descriptor does not belong to state %s", state)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func borrowStart(int) bool { return false }
