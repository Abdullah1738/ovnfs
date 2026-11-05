package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stencil-hq/ovnfs/store"
	"golang.org/x/sys/windows"
)

// TestCaptureSymlinkSlashes stores a link target with '/' separators, as git
// trees hold them, so peers on other platforms resolve it.
func TestCaptureSymlinkSlashes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "d", "top"), "x")
	if err := os.MkdirAll(filepath.Join(dir, "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(`..\d\top`, filepath.Join(dir, "e", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res := capture(t, dir, nil, "")
	if got, want := res.Files["e/link"].ID, store.HashBytes("blob", []byte("../d/top")); got != want {
		t.Fatalf("link captured as %s, want the id of %q", got, "../d/top")
	}
	if size := res.Info["e/link"].Size; size != int64(len("../d/top")) {
		t.Fatalf("link size %d", size)
	}
}

// TestCaptureFileID records the NTFS file id that the base view compares, so an
// unchanged file can be served from the folder.
func TestCaptureFileID(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "a.txt")
	write(t, name, "a")
	res := capture(t, dir, nil, "")
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		t.Fatal(err)
	}
	want := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	if got := res.Info["a.txt"].Ino; got == 0 || got != want {
		t.Fatalf("captured ino %#x, want file id %#x", got, want)
	}
}
