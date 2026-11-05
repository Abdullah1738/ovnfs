package overlay

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// A client must not mistake normal writes or memory writeback for replacement
// of an open file: the birth time stays fixed while data and mtime change.
func TestMemoryCreationTimeSurvivesChanges(t *testing.T) {
	f, err := New(t.TempDir(), nil, nil, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	h, err := f.OpenFile("archive.tmp", os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	defer h.Close()
	_, err = h.Write([]byte("before"))
	must(t, err)
	info, err := f.Lstat("archive.tmp")
	must(t, err)
	created := info.Sys().(*syscall.Stat_t).Birthtimespec
	assertCreated := func(name string) {
		t.Helper()
		info, err := f.Lstat(name)
		must(t, err)
		if got := info.Sys().(*syscall.Stat_t).Birthtimespec; got != created {
			t.Fatalf("creation time moved from %v to %v", created, got)
		}
	}
	_, err = h.Write([]byte(" after"))
	must(t, err)
	stamp := time.Unix(1234567890, 123456789)
	must(t, f.Chtimes("archive.tmp", stamp, stamp))
	assertCreated("archive.tmp")
	must(t, f.Rename("archive.tmp", "archive.rlib"))
	assertCreated("archive.rlib")
	must(t, f.Flush())
	assertCreated("archive.rlib")
	if got := read(t, f, "archive.rlib"); got != "before after" {
		t.Fatalf("renamed contents %q", got)
	}
	info, err = f.Lstat("archive.rlib")
	must(t, err)
	if !info.ModTime().Equal(stamp) {
		t.Fatalf("writeback changed mtime from %v to %v", stamp, info.ModTime())
	}
}
