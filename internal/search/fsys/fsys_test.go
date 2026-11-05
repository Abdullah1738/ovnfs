package fsys

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"
)

func listNames(t *testing.T, s Session, h Handle) []string {
	t.Helper()
	ls, err := s.List(h, Full, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range ls.Entries {
		names = append(names, string(ls.Name(&ls.Entries[i]))+":"+ls.Entries[i].Type.String())
	}
	slices.Sort(names)
	return names
}

// readFile opens name below dir and reads up to limit bytes of it into buf.
func readFile(s Session, dir Handle, name string, buf []byte, limit int64) ([]byte, FileInfo, error) {
	h, info, err := s.OpenFile(dir, name)
	if err != nil {
		return nil, info, err
	}
	defer s.Close(h)
	data, err := s.Read(h, buf, limit)
	return data, info, err
}

// Both filesystems must answer the same operations the same way.
func TestNativeAndIOFSAgree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a.txt": "hello", "dir/b.txt": "0123456789"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mem := fstest.MapFS{
		"a.txt":     {Data: []byte("hello")},
		"dir/b.txt": {Data: []byte("0123456789")},
	}
	for _, c := range []struct {
		name string
		fs   FS
		root string
	}{{"native", Native, root}, {"iofs", FromIOFS(mem), "/"}} {
		t.Run(c.name, func(t *testing.T) {
			abs, err := c.fs.Abs(c.root)
			if err != nil {
				t.Fatal(err)
			}
			s := c.fs.NewSession()
			h, err := s.OpenDir(abs)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(h)
			if got, want := listNames(t, s, h), []string{"a.txt:file", "dir:dir"}; !slices.Equal(got, want) {
				t.Errorf("list: got %q, want %q", got, want)
			}

			data, info, err := readFile(s, h, "dir/b.txt", nil, 4)
			if err != nil || string(data) != "0123" || info.Size != 10 {
				t.Errorf("limited read: %q size=%d err=%v", data, info.Size, err)
			}
			data, info, err = readFile(s, h, "a.txt", data, 1<<20)
			if err != nil || string(data) != "hello" || info.Size != 5 {
				t.Errorf("full read: %q size=%d err=%v", data, info.Size, err)
			}
			if _, _, err := s.OpenFile(h, "dir"); !errors.Is(err, ErrNotRegular) {
				t.Errorf("opening a directory: err=%v, want ErrNotRegular", err)
			}
			if _, _, err := s.OpenFile(h, "missing"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("opening a missing file: err=%v, want not-exist", err)
			}

			child, err := s.OpenChild(h, "dir")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(child)
			if got, want := listNames(t, s, child), []string{"b.txt:file"}; !slices.Equal(got, want) {
				t.Errorf("child list: got %q, want %q", got, want)
			}
		})
	}
}
