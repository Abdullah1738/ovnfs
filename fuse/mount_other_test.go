//go:build !linux

package fuse

import (
	"runtime"
	"strings"
	"testing"
)

// TestMountUnsupported checks that non-Linux builds refuse to mount with the OS named.
func TestMountUnsupported(t *testing.T) {
	srv, err := Mount(nil, t.TempDir(), Options{})
	if srv != nil || err == nil || !strings.Contains(err.Error(), "unsupported on "+runtime.GOOS) {
		t.Fatalf("Mount = %v, %v; want unsupported on %s", srv, err, runtime.GOOS)
	}
}
