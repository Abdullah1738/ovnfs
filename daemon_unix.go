//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a running process, including one
// owned by another user.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// detachedProcess puts the daemon in its own session so it outlives the invoking terminal.
func detachedProcess() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
