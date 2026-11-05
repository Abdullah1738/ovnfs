package export

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNodeLogRestart preserves moved descendants and the allocation watermark.
func TestNodeLogRestart(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	nt, err := openNodeTable(name)
	if err != nil {
		t.Fatal(err)
	}
	dir, child := nt.id("co/a"), nt.id("co/a/file")
	victim := nt.id("co/b")
	retired := nt.id("removed/deep/file")
	nt.rename("co/a", "co/new/b")
	nt.remove("removed")
	nt.remove("co/b")
	if err := nt.close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		nt, err = openNodeTable(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := pathOf(t, nt, dir); got != "co/new/b" {
			t.Fatalf("directory: %s", got)
		}
		if got := pathOf(t, nt, child); got != "co/new/b/file" {
			t.Fatalf("child: %s", got)
		}
		for _, id := range []uint64{victim, retired} {
			if _, ok := nt.path(id); ok {
				t.Fatalf("retired %d restored", id)
			}
		}
		if nt.id("co/new/b/file") != child {
			t.Fatal("identity changed")
		}
		if id := nt.id("fresh"); id <= retired {
			t.Fatalf("counter reused %d", id)
		}
		if err := nt.close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNodeLogTruncatedTail recovers complete operations before an interrupted write.
func TestNodeLogTruncatedTail(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	nt, err := openNodeTable(name)
	if err != nil {
		t.Fatal(err)
	}
	id := nt.id("co/file")
	if err := nt.close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"op":"alloc"`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	nt, err = openNodeTable(name)
	if err != nil {
		t.Fatal(err)
	}
	defer nt.close()
	if got := pathOf(t, nt, id); got != "co/file" {
		t.Fatalf("path = %q", got)
	}
}
