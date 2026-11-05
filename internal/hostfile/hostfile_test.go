package hostfile

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenFilesStayMovable checks Unix semantics for files held open, which
// the daemon needs on Windows: clients rename, replace and delete files that
// other clients (or cached SMB handles) still have open.
func TestOpenFilesStayMovable(t *testing.T) {
	dir := t.TempDir()
	path := func(name string) string { return filepath.Join(dir, name) }
	for name, data := range map[string]string{"target": "old", "src": "new"} {
		if err := os.WriteFile(path(name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	replaced, err := OpenFile(path("target"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer replaced.Close()
	if err := Rename(path("src"), path("target")); err != nil {
		t.Fatalf("replace an open file: %v", err)
	}
	if got := readAt(t, replaced); got != "old" {
		t.Fatalf("replaced handle reads %q, want the old content", got)
	}
	if got, _ := os.ReadFile(path("target")); string(got) != "new" {
		t.Fatalf("target holds %q, want the new content", got)
	}

	held, err := Open(path("target"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := Rename(path("target"), path("moved")); err != nil {
		t.Fatalf("rename an open file: %v", err)
	}
	if err := os.Remove(path("moved")); err != nil {
		t.Fatalf("delete an open file: %v", err)
	}
	if err := os.WriteFile(path("moved"), []byte("again"), 0o644); err != nil {
		t.Fatalf("reuse a deleted open file's name: %v", err)
	}
	if got := readAt(t, held); got != "new" {
		t.Fatalf("deleted handle reads %q, want its content", got)
	}
}

// TestRenameReplacesReadOnlyTarget checks the Unix rule that replacing a file
// needs write access to its directory, not to the file: Windows refuses
// read-only targets unless asked to ignore the attribute.
func TestRenameReplacesReadOnlyTarget(t *testing.T) {
	dir := t.TempDir()
	src, target := filepath.Join(dir, "src"), filepath.Join(dir, "target")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := Rename(src, target); err != nil {
		t.Fatalf("replace a read-only file: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Fatalf("target holds %q, want the new content", got)
	}
}

// TestRemoveKeepsReadOnlyHardLink checks that deleting one name of a read-only
// file leaves its other names read-only.
func TestRemoveKeepsReadOnlyHardLink(t *testing.T) {
	dir := t.TempDir()
	name, alias := filepath.Join(dir, "name"), filepath.Join(dir, "alias")
	if err := os.WriteFile(name, []byte("data"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(name, alias); err != nil {
		t.Fatal(err)
	}
	if err := Remove(alias); err != nil {
		t.Fatalf("remove a read-only file: %v", err)
	}
	if _, err := os.Lstat(alias); !os.IsNotExist(err) {
		t.Fatalf("removed name still exists: %v", err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("surviving name became writable: %v", info.Mode())
	}
}

// readAt reads a whole small file through f.
func readAt(t *testing.T, f *os.File) string {
	t.Helper()
	buf := make([]byte, 16)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return string(buf[:n])
}
