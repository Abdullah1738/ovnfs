//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockStart waits for the state's start lock, serializing concurrent starts;
// closing the returned file releases it.
func lockStart(state string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(state, startLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// detachedProcess puts the daemon in its own session so it outlives the invoking terminal.
func detachedProcess() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
