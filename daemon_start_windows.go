//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Windows range locks do not transfer to inherited handles. The parent keeps
// its lease until Ready, and terminates only its own failed startup child
// before releasing exclusion, so a canceled child cannot resume into a race.
func inheritStart(*exec.Cmd, *os.File) int { return 0 }
func inheritedStartLease(fd int, _ string) (*os.File, error) {
	if fd != 0 {
		return nil, fmt.Errorf("inherited startup descriptors are unsupported on Windows")
	}
	return nil, nil
}
func borrowStart(pid int) bool { return pid > 0 && pid == os.Getppid() }
func failedStart(cmd *exec.Cmd, exited <-chan error) {
	_ = cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
	}
}
