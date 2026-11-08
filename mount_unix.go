//go:build !windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// smbClientPort is 0: mount_smbfs and mount.cifs dial any port.
func smbClientPort() int { return 0 }

// mountRoots is empty: Unix kernels resolve absolute links through the mount.
func mountRoots(string) []string { return nil }

// smbAccount is empty: mount_smbfs and mount.cifs connect as guest.
func smbAccount(string) (user, password string, err error) { return "", "", nil }

// mountServer mounts the localhost NFS or SMB server at mount with the system
// client, updating access times only if atime is set.
func mountServer(proto string, port int, mount, user, password string, atime bool) error {
	cmd := mountCmd(proto, port, mount, atimeOption(atime), "ovnfs", user, password)
	if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// matchAtime brings the mount at dir, left by a previous daemon, to the
// access-time setting in place, so processes using the mount keep running.
func matchAtime(dir string, atime bool) error {
	if current, ok := mountAtime(dir); !ok || current == atime {
		return nil
	}
	cmd := remountCmd(dir, atimeOption(atime))
	if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

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
		return errors.New(strings.TrimSpace(string(out)))
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
