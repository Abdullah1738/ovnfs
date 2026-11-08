package clone

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// copiers are the routes Tree takes: one clonefile on APFS (a walk elsewhere),
// a walk cloning file by file, and a walk copying bytes.
var copiers = map[string]func(src, dst string) error{
	"tree": Tree,
	"walk": func(src, dst string) error { return walkFrom(&walk{}, src, dst) },
	"copy": func(src, dst string) error { return walkFrom(&walk{copying: true}, src, dst) },
}

// walkFrom runs w over the hierarchy at src.
func walkFrom(w *walk, src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	return w.tree(src, dst, info)
}

// TestTree checks each route keeps content, permission bits, modification
// times and symlinks, fills read-only directories, and refuses an existing dst.
func TestTree(t *testing.T) {
	mtime := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	src := filepath.Join(t.TempDir(), "src")
	ro := filepath.Join(src, "ro")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("tree"), 50000)
	if err := os.WriteFile(filepath.Join(src, "tool"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "inner"), []byte("inner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]os.FileMode{filepath.Join(src, "tool"): 0o751, filepath.Join(ro, "inner"): 0o444} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	symlinks := os.Symlink("tool", filepath.Join(src, "link")) == nil
	if !symlinks && runtime.GOOS != "windows" {
		t.Fatal("symlink failed")
	}
	if symlinks {
		if err := hostfile.Lchtimes(filepath.Join(src, "link"), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(src, "tool"), filepath.Join(ro, "inner"), ro, src} {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })

	for name, copier := range copiers {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "dst")
			if err := copier(src, dst); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(filepath.Join(dst, "ro"), 0o755) })
			if got, err := os.ReadFile(filepath.Join(dst, "tool")); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("tool: %d bytes, %v", len(got), err)
			}
			if got, err := os.ReadFile(filepath.Join(dst, "ro", "inner")); err != nil || string(got) != "inner\n" {
				t.Fatalf("ro/inner: %q, %v", got, err)
			}
			want := map[string]os.FileMode{"tool": 0o751, "ro": 0o555, "ro/inner": 0o444}
			for _, p := range []string{".", "tool", "ro", "ro/inner", "link"} {
				if p == "link" && !symlinks {
					continue
				}
				info, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(p)))
				if err != nil {
					t.Fatal(err)
				}
				if !info.ModTime().Equal(mtime) {
					t.Errorf("%s mtime %v, want %v", p, info.ModTime(), mtime)
				}
				if mode, ok := want[p]; ok && runtime.GOOS != "windows" && info.Mode().Perm() != mode {
					t.Errorf("%s mode %v, want %v", p, info.Mode().Perm(), mode)
				}
			}
			if symlinks {
				if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "tool" {
					t.Fatalf("link -> %q, %v", target, err)
				}
			}
			if err := copier(src, dst); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("copy onto existing dst = %v", err)
			}
		})
	}
}

// TestFD checks the clone carries the whole file and the requested mode, independent of the descriptor offset.
func TestFD(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	data := bytes.Repeat([]byte("0123456789"), 10000)
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := io.ReadFull(in, make([]byte, 123)); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := FD(in, dst, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("clone content differs: %d bytes, want %d", len(got), len(data))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v, want 0600", info.Mode().Perm())
		}
	}
	if err := FD(in, dst, 0o600); err == nil {
		t.Fatal("cloning onto an existing path succeeded")
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatal("failed clone modified the existing destination")
	}
}

// TestFile checks permission bits survive and non-regular sources are refused.
func TestFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tool")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o751); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy")
	if err := File(src, dst); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o751 {
			t.Fatalf("mode %v, want 0751", info.Mode().Perm())
		}
	}
	if err := File(dir, filepath.Join(dir, "dircopy")); err == nil {
		t.Fatal("cloning a directory succeeded")
	}
	if err := File(filepath.Join(dir, "missing"), filepath.Join(dir, "x")); err == nil {
		t.Fatal("cloning a missing file succeeded")
	}
}
