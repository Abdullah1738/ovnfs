//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// smbClientPort is 0: mount_smbfs and mount.cifs dial any port.
func smbClientPort() int { return 0 }

// makeMountPoint creates the directory the mount covers.
func makeMountPoint(dir string) error { return os.MkdirAll(dir, 0o755) }

// unmount detaches either protocol; an already removed mount counts as success.
func unmount(dir string, force bool) error {
	if !mounted(dir) {
		return nil
	}
	args := []string{dir}
	if force {
		args = []string{"-f", dir}
	}
	out, err := exec.Command("umount", args...).CombinedOutput()
	if err != nil {
		return errors.New(string(out))
	}
	return nil
}

// mountPath resolves aliases in the parent without entering a possibly hung mount.
func mountPath(dir string) string {
	dir = filepath.Clean(dir)
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return dir
	}
	return filepath.Join(parent, filepath.Base(dir))
}
