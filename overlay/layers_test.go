package overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tree writes a host tree, giving files and parent directories stable metadata.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, data := range files {
		real := filepath.Join(dir, p)
		if strings.HasSuffix(p, "/") {
			must(t, os.MkdirAll(real, 0o755))
			continue
		}
		must(t, os.MkdirAll(filepath.Dir(real), 0o755))
		must(t, os.WriteFile(real, []byte(data), 0o644))
	}
	must(t, filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, past, past)
	}))
	return dir
}

// stackFixture builds three immutable layers over a plain directory bottom.
func stackFixture(t *testing.T) *FS {
	t.Helper()
	bottom := tree(t, map[string]string{
		"d/base": "bottom", "d/replace": "bottom", "d/gone": "gone", "d/sub/base": "nested bottom",
		"opaque/base": "hidden", "block/child": "hidden", "through/base": "hidden", "deleted/child": "hidden",
	})
	low := tree(t, map[string]string{"d/low": "low", "d/replace": "low", "d/sub/low": "nested low", "opaque/low": "hidden"})
	mid := tree(t, map[string]string{"d/mid": "mid", "d/.wh.gone": "", "d/sub/mid": "nested mid", "opaque/.wh..wh..opq": "", "opaque/mid": "mid", "block": "file", "through/.wh..wh..opq": "", ".wh.deleted": ""})
	top := tree(t, map[string]string{"d/top": "top", "d/replace": "top", "d/sub/top": "nested top", "opaque/top": "top", "through/top": "top"})
	var layers []*Layer
	for _, dir := range []string{top, mid, low} {
		l, err := IndexLayer(dir)
		must(t, err)
		layers = append(layers, l)
	}
	f, err := New(t.TempDir(), layers, dirBottom{bottom}, nil)
	must(t, err)
	return f
}

// TestThreeLayerMerge exercises precedence, whiteouts and every ancestor barrier.
func TestThreeLayerMerge(t *testing.T) {
	f := stackFixture(t)
	for _, tc := range []struct {
		p    string
		want []string
	}{
		{"d", []string{"base", "low", "mid", "replace", "sub", "top"}},
		{"d/sub", []string{"base", "low", "mid", "top"}},
		{"opaque", []string{"mid", "top"}},
		{"through", []string{"top"}},
	} {
		t.Run(tc.p, func(t *testing.T) { assertNames(t, f, tc.p, tc.want...) })
	}
	for _, p := range []string{"d/gone", "opaque/base", "opaque/low", "block/child", "through/base", "deleted", "deleted/child"} {
		assertGone(t, f, p)
	}
	if got := read(t, f, "d/replace"); got != "top" {
		t.Fatalf("precedence = %q", got)
	}
}

// TestCopyUpEveryLevel preserves data and metadata from each possible source.
func TestCopyUpEveryLevel(t *testing.T) {
	for _, p := range []string{"d/top", "d/mid", "d/low", "d/base"} {
		t.Run(p, func(t *testing.T) {
			f := stackFixture(t)
			before := read(t, f, p)
			must(t, f.Chmod(p, 0o750))
			if read(t, f, p) != before {
				t.Fatal("copy-up changed data")
			}
			for _, q := range []string{"", "d", p} {
				info, err := f.Lstat(q)
				must(t, err)
				if !info.ModTime().Equal(past) {
					t.Fatalf("%q mtime = %v", q, info.ModTime())
				}
			}
			info, err := f.Lstat(p)
			must(t, err)
			if info.Mode().Perm() != 0o750 {
				t.Fatal("copy-up lost requested mode")
			}
		})
	}
}

// TestRenameEveryLevel moves files supplied by every layer and the bottom.
func TestRenameEveryLevel(t *testing.T) {
	for _, p := range []string{"d/top", "d/mid", "d/low", "d/base"} {
		t.Run(p, func(t *testing.T) {
			f := stackFixture(t)
			before := read(t, f, p)
			must(t, f.Rename(p, "renamed"))
			assertGone(t, f, p)
			if got := read(t, f, "renamed"); got != before {
				t.Fatalf("renamed content = %q", got)
			}
			info, err := f.Lstat("renamed")
			must(t, err)
			if !info.ModTime().Equal(past) {
				t.Fatalf("renamed mtime = %v", info.ModTime())
			}
			assertIndexMatchesDisk(t, f)
		})
	}
}

// TestThreeLayerDirectoryRename materializes every visible level but no hidden names.
func TestThreeLayerDirectoryRename(t *testing.T) {
	f := stackFixture(t)
	write(t, f, "d/up", "upper", os.O_CREATE|os.O_WRONLY)
	must(t, f.Rename("d", "moved"))
	assertGone(t, f, "d")
	assertNames(t, f, "moved", "base", "low", "mid", "replace", "sub", "top", "up")
	assertNames(t, f, "moved/sub", "base", "low", "mid", "top")
	assertGone(t, f, "moved/gone")
	for _, p := range []string{"base", "low", "mid", "top"} {
		if got := read(t, f, "moved/sub/"+p); got != "nested "+map[string]string{"base": "bottom", "low": "low", "mid": "mid", "top": "top"}[p] {
			t.Fatalf("copied %s = %q", p, got)
		}
	}
	must(t, f.MkdirAll("d", 0o755))
	assertNames(t, f, "d")
	must(t, f.Rename("moved/top", "newname"))
	info, err := f.Lstat("newname")
	must(t, err)
	if info.Name() != "newname" {
		t.Fatalf("renamed info name = %q", info.Name())
	}
}

// TestLayerDeletionAndRecreation never revives a directory hidden by any layer.
func TestLayerDeletionAndRecreation(t *testing.T) {
	f := stackFixture(t)
	for _, p := range []string{"d/top", "d/mid", "d/low", "d/base"} {
		must(t, f.Remove(p))
		assertGone(t, f, p)
		if _, err := os.Stat(f.whiteoutPath(p)); err != nil {
			t.Fatalf("missing marker for %s: %v", p, err)
		}
	}
	must(t, f.MkdirAll("deleted", 0o755))
	assertNames(t, f, "deleted")
	if err := f.MkdirAll("block/child", 0o755); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("non-directory parent error = %v", err)
	}
}

// TestLayerIndex verifies marker conversion, snapshots and stable preorder walks.
func TestLayerIndex(t *testing.T) {
	dir := tree(t, map[string]string{"a/.wh.x": "", "a/.wh..wh..opq": "", "a/file": "x", "z/": ""})
	must(t, os.Symlink("file", filepath.Join(dir, "a/link")))
	l, err := IndexLayer(dir)
	must(t, err)
	for _, tc := range []struct {
		p      string
		kind   Kind
		opaque bool
	}{
		{"", KindDir, false}, {"a", KindDir, true}, {"a/x", KindWhiteout, false},
		{"a/file", KindFile, false}, {"a/link", KindSymlink, false}, {"z", KindDir, false},
	} {
		e, ok := l.Get(tc.p)
		if !ok || e.Kind != tc.kind || e.Opaque != tc.opaque {
			t.Fatalf("Get(%q) = %+v, %v", tc.p, e, ok)
		}
		if (e.Info == nil) != (e.Kind == KindWhiteout) {
			t.Fatal("whiteout metadata invariant")
		}
	}
	children := l.Children("a")
	delete(children, "file")
	if _, ok := l.Children("a")["file"]; !ok {
		t.Fatal("Children exposed mutable index")
	}
	var paths []string
	must(t, l.Walk(func(p string, e Entry) error { paths = append(paths, p); return nil }))
	if want := []string{"", "a", "a/file", "a/link", "a/x", "z"}; !slices.Equal(paths, want) {
		t.Fatalf("Walk = %v", paths)
	}
	sentinel := errors.New("stop")
	if err := l.Walk(func(string, Entry) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal("Walk ignored callback error")
	}
	must(t, os.Mkdir(filepath.Join(dir, workName), 0o700))
	if _, err := IndexLayer(dir); err == nil {
		t.Fatal("indexed unfrozen work directory")
	}
}

// TestRootOpaqueAndTypeReplacement truncates merges at root and type boundaries.
func TestRootOpaqueAndTypeReplacement(t *testing.T) {
	bottom := tree(t, map[string]string{"rootfile": "hidden", "d/old": "hidden", "file": "bottom"})
	mid, err := IndexLayer(tree(t, map[string]string{"d": "middle file", "file/child": "middle child"}))
	must(t, err)
	top, err := IndexLayer(tree(t, map[string]string{"d/new": "new", "file": "top file"}))
	must(t, err)
	f, err := New(t.TempDir(), []*Layer{top, mid}, dirBottom{bottom}, nil)
	must(t, err)
	assertNames(t, f, "d", "new")
	assertGone(t, f, "d/old")
	assertGone(t, f, "file/child")
	opaque, err := IndexLayer(tree(t, map[string]string{opaqueName: "", "visible": "yes"}))
	must(t, err)
	root, ok := opaque.Get("")
	if !ok || !root.Opaque || root.Kind != KindDir {
		t.Fatalf("opaque root = %+v", root)
	}
	f, err = New("", []*Layer{opaque, top, mid}, dirBottom{bottom}, nil)
	must(t, err)
	assertNames(t, f, "", "visible")
	assertGone(t, f, "rootfile")
	assertGone(t, f, "d")
}

// TestSharedLayersIsolateWritableUppers models a fork over the same frozen stack.
func TestSharedLayersIsolateWritableUppers(t *testing.T) {
	parent := stackFixture(t)
	child, err := New(t.TempDir(), parent.Stack(), parent.bottom, nil)
	must(t, err)
	write(t, parent, "d/low", "parent", os.O_WRONLY|os.O_TRUNC)
	if got := read(t, child, "d/low"); got != "low" {
		t.Fatal("parent write leaked into child:", got)
	}
	write(t, child, "d/mid", "child", os.O_WRONLY|os.O_TRUNC)
	if got := read(t, parent, "d/mid"); got != "mid" {
		t.Fatal("child write leaked into parent:", got)
	}
	must(t, child.Remove("d/top"))
	if got := read(t, parent, "d/top"); got != "top" {
		t.Fatal("child deletion leaked into parent:", got)
	}
	assertGone(t, child, "d/top")
}

// TestUpperIndexDirectoryAndSymlinkOperations indexes all non-file mutations.
func TestUpperIndexDirectoryAndSymlinkOperations(t *testing.T) {
	f := stackFixture(t)
	must(t, f.MkdirAll("new/nested", 0o750))
	assertIndexMatchesDisk(t, f)
	must(t, f.Symlink("../d/top", "new/link"))
	assertIndexMatchesDisk(t, f)
	target, err := f.Readlink("new/link")
	must(t, err)
	if target != "../d/top" {
		t.Fatal(target)
	}
	h, err := f.TempFile("new/nested", "tmp")
	must(t, err)
	_, err = h.Write([]byte("data"))
	must(t, err)
	must(t, h.Truncate(2))
	must(t, h.Close())
	assertIndexMatchesDisk(t, f)
	must(t, f.Rename("new", "renamed"))
	assertIndexMatchesDisk(t, f)
	must(t, f.Remove("renamed/link"))
	assertIndexMatchesDisk(t, f)
	must(t, f.Remove("d/top"))
	must(t, f.MkdirAll("deleted", 0o755))
	assertIndexMatchesDisk(t, f)
}

// TestCopyUpSymlinkPreservesTarget never dereferences links while materializing them.
func TestCopyUpSymlinkPreservesTarget(t *testing.T) {
	dir := tree(t, map[string]string{"file": "data"})
	must(t, os.Symlink("file", filepath.Join(dir, "link")))
	l, err := IndexLayer(dir)
	must(t, err)
	f, err := New(t.TempDir(), []*Layer{l}, nil, nil)
	must(t, err)
	must(t, f.Rename("link", "moved"))
	target, err := f.Readlink("moved")
	must(t, err)
	if target != "file" {
		t.Fatal(target)
	}
	info, err := f.Lstat("moved")
	must(t, err)
	original, err := os.Lstat(filepath.Join(dir, "link"))
	must(t, err)
	if info.Mode()&os.ModeSymlink == 0 || !info.ModTime().Equal(original.ModTime()) {
		t.Fatalf("symlink metadata changed: %v %v", info.Mode(), info.ModTime())
	}
	assertGone(t, f, "link")
}

// TestOpenResolvesMergedSymlinks keeps Bottom.Open regular-only and writes in upper.
func TestOpenResolvesMergedSymlinks(t *testing.T) {
	bottom := tree(t, map[string]string{"target": "bottom"})
	must(t, os.Symlink("target", filepath.Join(bottom, "bottom-link")))
	layerDir := tree(t, map[string]string{"layer-target": "layer"})
	must(t, os.Symlink("target", filepath.Join(layerDir, "layer-link")))
	must(t, os.Symlink("layer-target", filepath.Join(bottom, "cross-link")))
	must(t, os.Symlink("missing", filepath.Join(bottom, "dangling")))
	must(t, os.Symlink("cycle", filepath.Join(bottom, "cycle")))
	l, err := IndexLayer(layerDir)
	must(t, err)
	f, err := New(t.TempDir(), []*Layer{l}, dirBottom{bottom}, nil)
	must(t, err)
	for _, tc := range []struct{ p, want string }{
		{"bottom-link", "bottom"}, {"layer-link", "bottom"}, {"cross-link", "layer"},
	} {
		h, err := f.Open(tc.p)
		must(t, err)
		if h.Name() != tc.p {
			t.Fatalf("open name = %q, want %q", h.Name(), tc.p)
		}
		must(t, h.Close())
		if got := read(t, f, tc.p); got != tc.want {
			t.Fatalf("%s = %q", tc.p, got)
		}
	}
	write(t, f, "layer-link", "upper", os.O_WRONLY|os.O_TRUNC)
	if got := read(t, f, "target"); got != "upper" {
		t.Fatal(got)
	}
	data, err := os.ReadFile(filepath.Join(bottom, "target"))
	must(t, err)
	if string(data) != "bottom" {
		t.Fatal("symlink write changed bottom")
	}
	if _, err := f.OpenFile("dangling", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("dangling exclusive error = %v", err)
	}
	write(t, f, "dangling", "created", os.O_CREATE|os.O_WRONLY)
	if got := read(t, f, "missing"); got != "created" {
		t.Fatal(got)
	}
	if _, err := f.Open("cycle"); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("cycle error = %v", err)
	}
	assertIndexMatchesDisk(t, f)
}

// TestReadOnlyStack rejects every mutation while still reading indexed data.
func TestReadOnlyStack(t *testing.T) {
	f := stackFixture(t)
	ro, err := New("", f.Stack(), f.bottom, nil)
	must(t, err)
	if got := read(t, ro, "d/top"); got != "top" {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"create", func() error {
			h, err := ro.Create("new")
			if h != nil {
				h.Close()
			}
			return err
		}},
		{"open", func() error {
			h, err := ro.OpenFile("d/top", os.O_WRONLY, 0)
			if h != nil {
				h.Close()
			}
			return err
		}},
		{"temp", func() error {
			h, err := ro.TempFile("", "tmp")
			if h != nil {
				h.Close()
			}
			return err
		}},
		{"mkdir", func() error { return ro.MkdirAll("new", 0o755) }},
		{"remove", func() error { return ro.Remove("d/top") }},
		{"rename", func() error { return ro.Rename("d/top", "new") }},
		{"symlink", func() error { return ro.Symlink("target", "new") }},
		{"link", func() error { return ro.Link("d/top", "new") }},
		{"chmod", func() error { return ro.Chmod("d/top", 0o600) }},
		{"chown", func() error { return ro.Chown("d/top", 0, 0) }},
		{"lchown", func() error { return ro.Lchown("d/top", 0, 0) }},
		{"chtimes", func() error { return ro.Chtimes("d/top", past, past) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, syscall.EROFS) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// TestHardLinkCopiesUp verifies shared upper inodes never alter immutable data.
func TestHardLinkCopiesUp(t *testing.T) {
	f := stackFixture(t)
	must(t, f.Link("d/low", "linked"))
	a, err := os.Stat(f.up("d/low"))
	must(t, err)
	b, err := os.Stat(f.up("linked"))
	must(t, err)
	if !os.SameFile(a, b) {
		t.Fatal("link does not share an upper inode")
	}
	write(t, f, "linked", "X", os.O_WRONLY)
	if got := read(t, f, "d/low"); got != "Xow" {
		t.Fatal(got)
	}
	original, err := os.ReadFile(filepath.Join(f.layers[2].Dir(), "d/low"))
	must(t, err)
	if string(original) != "low" {
		t.Fatal("hard link modified immutable layer")
	}
	if err := f.Link("d/low", "linked"); !errors.Is(err, fs.ErrExist) {
		t.Fatal(err)
	}
	must(t, f.Remove("d/base"))
	must(t, f.Link("d/low", "d/base"))
	if got := read(t, f, "d/base"); got != "Xow" {
		t.Fatal(got)
	}
}

// TestUpperIndexRandomOperations compares every structural update with a disk walk.
func TestUpperIndexRandomOperations(t *testing.T) {
	f := stackFixture(t)
	rng := rand.New(rand.NewPCG(7, 19))
	for i := range 300 {
		p := fmt.Sprintf("random/n%d", rng.IntN(16))
		must(t, f.MkdirAll("random", 0o755))
		switch rng.IntN(6) {
		case 0, 1:
			write(t, f, p, fmt.Sprint(i), os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
		case 2:
			err := f.Remove(p)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		case 3:
			err := f.Rename(p, p+"m")
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		case 4:
			err := f.Chmod(p, 0o600)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		case 5:
			err := f.Link(p, p+"l")
			if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrExist) {
				t.Fatal(err)
			}
		}
		assertIndexMatchesDisk(t, f)
	}
	must(t, f.Rename("random", "renamed"))
	assertIndexMatchesDisk(t, f)
	must(t, f.Remove("d/top"))
	assertIndexMatchesDisk(t, f)
}

// assertIndexMatchesDisk checks paths, kinds, opacity, child maps and inode identity.
func assertIndexMatchesDisk(t *testing.T, f *FS) {
	t.Helper()
	disk, err := indexLayer(f.upper, true)
	must(t, err)
	if len(disk.entries) != len(f.index.entries) {
		t.Fatalf("entry count disk=%d index=%d", len(disk.entries), len(f.index.entries))
	}
	for p, want := range disk.entries {
		got, ok := f.index.entries[p]
		if !ok || got.Kind != want.Kind || got.Opaque != want.Opaque {
			t.Fatalf("index mismatch at %s: %+v != %+v", p, got, want)
		}
		if want.Info != nil && (!sameFile(got.Info, want.Info) || got.Info.Name() != want.Info.Name()) {
			t.Fatalf("stale inode or name at %s", p)
		}
		if !reflect.DeepEqual(slices.Sorted(maps.Keys(disk.children[p])), slices.Sorted(maps.Keys(f.index.children[p]))) {
			t.Fatalf("child map mismatch at %s", p)
		}
	}
}

// TestHundredLayerLookupBounds keeps deep lookups indexed rather than probing disk.
func TestHundredLayerLookupBounds(t *testing.T) {
	base := tree(t, map[string]string{"deep/file": "base"})
	layers := make([]*Layer, 0, 100)
	for i := range 100 {
		l, err := IndexLayer(tree(t, map[string]string{fmt.Sprintf("checkpoint%d", i): "data"}))
		must(t, err)
		layers = append(layers, l)
	}
	f, err := New("", layers, dirBottom{base}, nil)
	must(t, err)
	start := time.Now()
	for range 5000 {
		info, err := f.Lstat("deep/file")
		must(t, err)
		if info.Size() != 4 {
			t.Fatal("wrong bottom entry")
		}
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("indexed 100-layer lookup took %v for 5000 calls", elapsed)
	}
	// The stack accessor must not let a caller reorder this filesystem's layers.
	stack := f.Stack()
	stack[0] = nil
	if f.Stack()[0] == nil {
		t.Fatal("Stack exposed mutable slice")
	}
}

// TestEmptyUsesMarkers verifies deletions and opaque directories count as changes.
func TestEmptyUsesMarkers(t *testing.T) {
	f := stackFixture(t)
	empty, err := f.Empty()
	must(t, err)
	if !empty {
		t.Fatal("fresh upper is not empty")
	}
	must(t, f.Remove("d/top"))
	empty, err = f.Empty()
	must(t, err)
	if empty {
		t.Fatal("whiteout was ignored")
	}
}

// TestCopyUpDirectoryMode preserves parent permissions as well as modification time.
func TestCopyUpDirectoryMode(t *testing.T) {
	bottom := tree(t, map[string]string{"d/f": "data"})
	must(t, os.Chmod(filepath.Join(bottom, "d"), 0o750))
	f, err := New(t.TempDir(), nil, dirBottom{bottom}, nil)
	must(t, err)
	must(t, f.Chmod("d/f", 0o600))
	info, err := f.Lstat(path.Dir("d/f"))
	must(t, err)
	// A Windows host directory cannot hold 0750, so the lower never had it.
	if info.Mode().Perm() != 0o750 && runtime.GOOS != "windows" || !info.ModTime().Equal(past) {
		t.Fatalf("parent metadata = %v %v", info.Mode(), info.ModTime())
	}
}
