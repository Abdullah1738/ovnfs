//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMountPathResolvesOnlyParent protects restart from inspecting the mount itself.
func TestMountPathResolvesOnlyParent(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "mount")
	if err := os.Symlink("unreachable-target", target); err != nil {
		t.Fatal(err)
	}
	if got := mountPath(filepath.Join(alias, "mount")); got != target {
		t.Fatalf("mountPath followed the mount target: got %q, want %q", got, target)
	}
	if mounted(filepath.Join(alias, "mount")) {
		t.Fatal("unmounted target reported as mounted")
	}
}
