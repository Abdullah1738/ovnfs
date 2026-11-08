package dirbottom

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// TestDirectoryBottom exercises host reads and recursive copy-up without changing the lower.
func TestDirectoryBottom(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(lower, "dir"), 0750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(lower, "dir", "file")
	if err := os.WriteFile(file, []byte("lower"), 0751); err != nil {
		t.Fatal(err)
	}
	when := time.Unix(1_000_000_000, 123456789)
	for _, path := range []string{file, filepath.Dir(file)} {
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	b := New(lower)
	entries, err := b.ReadDir("dir")
	if err != nil || len(entries) != 1 || entries[0].Name() != "file" {
		t.Fatalf("ReadDir = %v, %v", entries, err)
	}
	in, err := b.Open("dir/file")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(in)
	if closeErr := in.Close(); err == nil {
		err = closeErr
	}
	if err != nil || string(data) != "lower" {
		t.Fatalf("Open = %q, %v", data, err)
	}
	if err := b.CopyTo("dir", filepath.Join(upper, "copy")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dir", "dir/file"} {
		src, err := b.Lstat(name)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(upper, "copy")
		if name == "dir/file" {
			dst = filepath.Join(dst, "file")
		}
		copied, err := os.Lstat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if !copied.ModTime().Equal(src.ModTime()) {
			t.Fatalf("%s mtime = %v, want %v", name, copied.ModTime(), src.ModTime())
		}
		if runtime.GOOS != "windows" && copied.Mode() != src.Mode() {
			t.Fatalf("%s mode = %v, want %v", name, copied.Mode(), src.Mode())
		}
	}
	if err := os.WriteFile(filepath.Join(upper, "copy", "file"), []byte("upper"), 0644); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(file)
	if err != nil || string(data) != "lower" {
		t.Fatalf("copy changed source: %q, %v", data, err)
	}
	if _, err := b.Lstat("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing entry: %v", err)
	}
}

// TestSymlinkCopy preserves the link itself and never updates the destination target's timestamps.
func TestSymlinkCopy(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	link := filepath.Join(lower, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	when := time.Unix(1_000_000_000, 0)
	if err := hostfile.Lchtimes(link, when, when); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(upper, "target")
	if err := os.WriteFile(target, []byte("target"), 0644); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	b := New(lower)
	if _, err := b.Open("link"); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("Open followed a symlink: %v", err)
	}
	if err := b.CopyTo("link", filepath.Join(upper, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(upper, "link"))
	if err != nil || got != "target" {
		t.Fatalf("copied target = %q, %v", got, err)
	}
	copied, err := os.Lstat(filepath.Join(upper, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if copied.Mode()&os.ModeSymlink == 0 || !copied.ModTime().Equal(when) {
		t.Fatalf("copied symlink metadata = %v, %v", copied.Mode(), copied.ModTime())
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(original.ModTime()) {
		t.Fatal("symlink copy changed target timestamp")
	}
}
