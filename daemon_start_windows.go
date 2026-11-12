//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// Windows range locks do not transfer to inherited handles. The parent keeps
// its lease until Ready; pendingStartupHolder excludes its child after exit.
func inheritStart(*exec.Cmd, *os.File) int { return 0 }
func inheritedStartLease(fd int, _ string) (*os.File, error) {
	if fd != 0 {
		return nil, fmt.Errorf("inherited startup descriptors are unsupported on Windows")
	}
	return nil, nil
}
func borrowStart(pid int) bool { return pid > 0 && pid == os.Getppid() }
