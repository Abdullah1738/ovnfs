//go:build !windows

package walk

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO where an ignore file is expected must be skipped, not opened for a
// read that blocks until some writer appears.
func TestFIFOIgnoreSourceDoesNotBlock(t *testing.T) {
	isolateGit(t)
	root := makeTree(t, ".git/info/", "kept.txt")
	if err := syscall.Mkfifo(filepath.Join(root, ".git/info/exclude"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	if err := os.Symlink(".git/info/exclude", filepath.Join(root, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	done := make(chan []string, 1)
	go func() { done <- paths(t, root, Options{Gitignore: true}) }()
	select {
	case got := <-done:
		wantPaths(t, got, "kept.txt")
	case <-time.After(5 * time.Second):
		t.Fatal("walk blocked opening a FIFO ignore source")
	}
}
