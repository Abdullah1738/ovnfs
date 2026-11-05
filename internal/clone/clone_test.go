package clone

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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
