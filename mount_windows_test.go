//go:build windows

package main

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestMakeMountPoint accepts only a free drive root, in either letter case.
func TestMakeMountPoint(t *testing.T) {
	system := os.Getenv("SystemDrive")
	for _, dir := range []string{system + `\`, strings.ToLower(system) + `\`, system + `\ovnfs`} {
		if err := makeMountPoint(dir); err == nil {
			t.Errorf("accepted %s", dir)
		}
	}
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	for letter := 'Z'; letter >= 'D'; letter-- {
		drive := string(letter) + ":"
		if remote, err := driveRemote(drive); drives&(1<<(letter-'A')) != 0 || err != nil || remote != "" {
			continue
		}
		for _, dir := range []string{drive + `\`, strings.ToLower(drive) + `\`} {
			if err := makeMountPoint(dir); err != nil {
				t.Errorf("free drive %s: %v", dir, err)
			}
			if mounted(dir) {
				t.Errorf("free drive %s reported mounted", dir)
			}
		}
		return
	}
	t.Skip("no free drive letter")
}
