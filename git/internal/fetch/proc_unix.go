//go:build !windows

package fetch

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// setProcessGroup starts the child in its own process group and makes
// cancellation kill the whole group: an ssh command run through the shell
// may leave ssh as a grandchild that would otherwise survive.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// findExecutable reports dir/name if it is an executable file.
func findExecutable(dir, name string) (string, bool) {
	p := filepath.Join(dir, name)
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return "", false
	}
	return p, true
}
