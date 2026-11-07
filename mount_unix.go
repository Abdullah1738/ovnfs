//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// smbClientPort is 0: mount_smbfs and mount.cifs dial any port.
func smbClientPort() int { return 0 }

// mountRoots is empty: Unix kernels resolve absolute links through the mount.
func mountRoots(string) []string { return nil }

// smbAccount is empty: mount_smbfs and mount.cifs connect as guest.
func smbAccount(string) (user, password string, err error) { return "", "", nil }

// mountServer mounts the localhost NFS or SMB server at mount with the system client.
func mountServer(proto string, port int, mount, user, password string) error {
	cmd := mountCmd(proto, port, mount, "", "ovnfs", user, password)
	if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
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
