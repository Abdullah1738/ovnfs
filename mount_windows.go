//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// smbRemote is the UNC path serve maps the mount drive to; drives mapped
// anywhere else are never treated as ours.
const smbRemote = `\\127.0.0.1\ovnfs`

// mountRoots are the names Windows tools give the mount's root: its drive and,
// once they resolve a path, the share net use maps it to.
func mountRoots(mount string) []string {
	return []string{filepath.VolumeName(mount) + "/", filepath.ToSlash(smbRemote) + "/"}
}

// procWNetGetConnection reads a drive's network mapping without spawning net use.
var procWNetGetConnection = windows.NewLazySystemDLL("mpr.dll").NewProc("WNetGetConnectionW")

// unmount removes this daemon's mapped SMB drive holding dir, suppressing the interactive prompt.
func unmount(dir string, _ bool) error {
	if !mounted(dir) {
		return nil
	}
	out, err := exec.Command("net", "use", filepath.VolumeName(dir), "/delete", "/y").CombinedOutput()
	if err != nil {
		return fmt.Errorf("net use /delete: %w: %s", err, out)
	}
	return nil
}

// mounted reports whether dir's drive is mapped to this daemon's share.
func mounted(dir string) bool {
	remote, err := driveRemote(filepath.VolumeName(dir))
	return err == nil && strings.EqualFold(remote, smbRemote)
}

// makeMountPoint checks that net use or WinFsp can take dir: it fails when dir
// is not a drive root, the drive is mapped to a share (connected or only
// remembered), or a volume holds the letter.
func makeMountPoint(dir string) error {
	drive := filepath.VolumeName(dir)
	if len(drive) != 2 || drive[1] != ':' || dir != drive+`\` {
		return fmt.Errorf("%s is not a drive root: checkouts on Windows mount on a free drive letter such as O:", dir)
	}
	remote, err := driveRemote(drive)
	if err != nil {
		return fmt.Errorf("reading drive %s mapping: %w", drive, err)
	}
	if remote != "" {
		return fmt.Errorf("drive %s is mapped to %s; remove it with \"net use %s /delete\" or choose another drive with --mount", drive, remote, drive)
	}
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return fmt.Errorf("listing drives: %w", err)
	}
	if drives&(1<<(strings.ToUpper(drive)[0]-'A')) != 0 {
		return fmt.Errorf("drive %s holds a local volume; choose another drive with --mount", drive)
	}
	return nil
}

// driveRemote returns the network path a drive such as "O:" is mapped to, or ""
// when it has none. A remembered mapping that is not connected still counts,
// since net use refuses to map over it.
func driveRemote(drive string) (string, error) {
	if err := procWNetGetConnection.Find(); err != nil {
		return "", err
	}
	local, err := windows.UTF16PtrFromString(drive)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n := uint32(len(buf))
		r, _, _ := procWNetGetConnection.Call(uintptr(unsafe.Pointer(local)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
		switch e := syscall.Errno(r); e {
		case windows.NO_ERROR, windows.ERROR_CONNECTION_UNAVAIL:
			return windows.UTF16ToString(buf), nil
		case windows.ERROR_NOT_CONNECTED:
			return "", nil
		case windows.ERROR_MORE_DATA:
			buf = make([]uint16, n)
		default:
			return "", e
		}
	}
}

// smbClientPort is the only port net use dials on this Windows build: 445
// before Windows 11 24H2 (build 26100) added /TCPPORT, otherwise 0 (any port).
func smbClientPort() int {
	if windows.RtlGetVersion().BuildNumber < 26100 {
		return 445
	}
	return 0
}
