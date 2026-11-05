package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestIndexSnapshotCopy preserves independent index snapshots and overwrite behavior.
func TestIndexSnapshotCopy(t *testing.T) {
	dir := t.TempDir()
	source, snapshot := filepath.Join(dir, "source"), filepath.Join(dir, "snapshot")
	testWrite(t, source, "original index bytes\n")
	if err := copyFile(source, snapshot); err != nil {
		t.Fatal(err)
	}
	testWrite(t, source, "new\n")
	data, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original index bytes\n" {
		t.Fatalf("snapshot changed with source: %q", data)
	}
	if err := copyFile(source, snapshot); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n" {
		t.Fatalf("overwrite retained stale bytes: %q", data)
	}
	testWrite(t, snapshot, "snapshot-only\n")
	data, err = os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n" {
		t.Fatalf("source changed with snapshot: %q", data)
	}
	if err := copyFile(filepath.Join(dir, "missing"), snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source = %v", err)
	}
	data, err = os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "snapshot-only\n" {
		t.Fatalf("missing source damaged destination: %q", data)
	}
}
