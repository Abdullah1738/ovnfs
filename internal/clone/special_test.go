//go:build darwin || linux

package clone

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestTreeSkipsSpecialFiles checks no route opens a fifo, which would block,
// that walks leave fifos out, and that a fifo at src copies nothing.
func TestTreeSkipsSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(src, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "file"), []byte("file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lone := filepath.Join(dir, "lone")
	if err := Tree(fifo, lone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(lone); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fifo at src copied: %v", err)
	}
	for name, copier := range copiers {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "dst")
			if err := copier(src, dst); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(dst, "file")); err != nil || string(got) != "file\n" {
				t.Fatalf("file: %q, %v", got, err)
			}
			// APFS's one-call clone keeps the fifo; every walk leaves it out.
			if _, err := os.Lstat(filepath.Join(dst, "fifo")); err == nil && name != "tree" {
				t.Fatal("walk copied the fifo")
			}
		})
	}
}
