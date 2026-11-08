package overlay

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/clone"
)

// memBlobs serves stored layer content from memory, by blob id.
type memBlobs map[string][]byte

// Open returns blob id's bytes.
func (b memBlobs) Open(id string) (Content, error) {
	data, ok := b[id]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: id, Err: fs.ErrNotExist}
	}
	return ByteContent(data), nil
}

// CopyTo writes blob id to the new file dst.
func (b memBlobs) CopyTo(id, dst string, perm os.FileMode) error {
	data, ok := b[id]
	if !ok {
		return &fs.PathError{Op: "copy", Path: id, Err: fs.ErrNotExist}
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := out.Write(data); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(perm); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// storedFixture is a raw frozen layer over a bottom, with the blobs and ids
// that store every file and symlink except the raw ones (keep.log, raw/dir/y).
type storedFixture struct {
	dir    string
	bottom dirBottom
	layer  *Layer
	blobs  memBlobs
	ids    map[string]string
}

// newStoredFixture builds the raw layer: files, an executable, a symlink, a
// hard link, nested and read-only directories, an opaque directory and a
// whiteout over the bottom.
func newStoredFixture(t *testing.T) *storedFixture {
	t.Helper()
	sf := &storedFixture{dir: filepath.Join(t.TempDir(), "layer"), bottom: dirBottom{tree(t, map[string]string{"gone": "bottom", "opq/hidden": "bottom", "src/base": "bottom"})}, blobs: memBlobs{}, ids: map[string]string{}}
	files := map[string]string{"src/a.txt": "alpha", "src/run.sh": "#!/bin/sh\n", "keep.log": "raw", "deep/er/x": "deep", "raw/dir/y": "raw too", "opq/z": "zed", "ro/f": "read-only parent"}
	for p, data := range files {
		host := filepath.Join(sf.dir, filepath.FromSlash(p))
		must(t, os.MkdirAll(filepath.Dir(host), 0o755))
		must(t, os.WriteFile(host, []byte(data), 0o644))
		if p != "keep.log" && p != "raw/dir/y" {
			sf.ids[p] = "blob-" + p
			sf.blobs["blob-"+p] = []byte(data)
		}
	}
	must(t, os.Chmod(filepath.Join(sf.dir, "src", "run.sh"), 0o755))
	must(t, os.Symlink("a.txt", filepath.Join(sf.dir, "src", "link")))
	sf.ids["src/link"], sf.blobs["blob-src/link"] = "blob-src/link", []byte("a.txt")
	must(t, os.Link(filepath.Join(sf.dir, "src", "a.txt"), filepath.Join(sf.dir, "src", "hard")))
	sf.ids["src/hard"] = "blob-src/a.txt"
	must(t, os.WriteFile(filepath.Join(sf.dir, ".wh.gone"), nil, 0o644))
	must(t, os.WriteFile(filepath.Join(sf.dir, "opq", opaqueName), nil, 0o644))
	must(t, os.Chmod(filepath.Join(sf.dir, "ro"), 0o555))
	t.Cleanup(func() { os.Chmod(filepath.Join(sf.dir, "ro"), 0o755) })
	var err error
	sf.layer, err = IndexLayer(sf.dir)
	must(t, err)
	return sf
}

// pathView is everything the overlay reports for one path.
type pathView struct {
	Name, Mode, Content string
	Size                int64
	MTime               int64
	Sys                 any
	Listed              bool // ReadDir reported the same metadata as Lstat
}

// viewOf records every visible path of f: Lstat and ReadDir metadata, native
// stat records (a layer's exactly), file contents and symlink targets.
func viewOf(t *testing.T, f *FS) map[string]pathView {
	t.Helper()
	out := map[string]pathView{}
	var visit func(p string, listed os.FileInfo)
	visit = func(p string, listed os.FileInfo) {
		info, err := f.Lstat(p)
		must(t, err)
		v := pathView{Name: info.Name(), Mode: info.Mode().String(), Size: info.Size(), MTime: info.ModTime().UnixNano(), Sys: info.Sys(), Listed: true}
		if f.InBottom(p) {
			v.Sys = stableSystemInfo(info) // the bottom is stat live, and reading it moves its access time
		}
		if listed != nil {
			v.Listed = listed.Name() == info.Name() && listed.Mode() == info.Mode() && listed.Size() == info.Size() && listed.ModTime().Equal(info.ModTime()) && reflect.DeepEqual(listed.Sys(), info.Sys())
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			v.Content, err = f.Readlink(p)
			must(t, err)
		case info.Mode().IsRegular():
			v.Content = read(t, f, p)
		case info.IsDir():
			items, err := f.ReadDir(p)
			must(t, err)
			for _, item := range items {
				visit(path.Join(p, item.Name()), item)
			}
		}
		out[p] = v
	}
	visit("", nil)
	return out
}

// assertSameView compares two views path by path.
func assertSameView(t *testing.T, got, want map[string]pathView) {
	t.Helper()
	for p, w := range want {
		if g, ok := got[p]; !ok || !reflect.DeepEqual(g, w) {
			t.Fatalf("%q: got %+v, want %+v", p, g, w)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Fatalf("%q appeared", p)
		}
	}
}

// layerFiles lists the regular files and symlinks left below dir.
func layerFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	must(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	}))
	slices.Sort(out)
	return out
}

// readOnlyView opens a read-only overlay of one layer over the fixture's bottom.
func (sf *storedFixture) readOnlyView(t *testing.T, l *Layer) *FS {
	t.Helper()
	f, err := New("", []*Layer{l}, sf.bottom, nil)
	must(t, err)
	return f
}

// TestStoredLayerServesTheSameView checks that storing a layer, and loading it
// again from its manifest, changes nothing the overlay reports, removes only
// stored content and markers from the layer directory, and keeps an open raw
// file readable.
func TestStoredLayerServesTheSameView(t *testing.T) {
	sf := newStoredFixture(t)
	raw := sf.readOnlyView(t, sf.layer)
	want := viewOf(t, raw)
	if _, ok := want["gone"]; ok {
		t.Fatal("whiteout did not hide the bottom")
	}
	if _, ok := want["opq/hidden"]; ok {
		t.Fatal("opaque directory did not hide the bottom")
	}
	open, err := raw.Open("src/a.txt")
	must(t, err)
	defer open.Close()

	must(t, sf.layer.Store(sf.ids, "tree", sf.blobs))
	if !sf.layer.Stored() || sf.layer.Tree() != "tree" {
		t.Fatalf("stored %v tree %q", sf.layer.Stored(), sf.layer.Tree())
	}
	if got := layerFiles(t, sf.dir); !slices.Equal(got, []string{manifestName, "keep.log", "raw/dir/y"}) {
		t.Fatalf("layer directory holds %v", got)
	}
	if data, err := io.ReadAll(open); err != nil || string(data) != "alpha" {
		t.Fatalf("open raw file reads %q %v", data, err)
	}
	assertSameView(t, viewOf(t, raw), want)

	reopened, err := OpenLayer(sf.dir, sf.blobs)
	must(t, err)
	if id, ok := reopened.Blob("src/hard"); !ok || id != "blob-src/a.txt" || reopened.Tree() != "tree" {
		t.Fatalf("reopened blob %q %v tree %q", id, ok, reopened.Tree())
	}
	assertSameView(t, viewOf(t, sf.readOnlyView(t, reopened)), want)
	if _, err := sf.readOnlyView(t, reopened).Open("src"); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("opening a stored directory: %v", err)
	}
}

// TestOpenLayerFinishesInterruptedStore checks recovery from a crash after the
// manifest was written but before raw files were removed, and that a layer
// whose manifest was never published is indexed raw.
func TestOpenLayerFinishesInterruptedStore(t *testing.T) {
	sf := newStoredFixture(t)
	want := viewOf(t, sf.readOnlyView(t, sf.layer))
	crashed := filepath.Join(t.TempDir(), "crashed")
	must(t, clone.Tree(sf.dir, crashed))
	must(t, os.WriteFile(filepath.Join(crashed, manifestName+".tmp"), []byte("{partial"), 0o600))
	unpublished, err := OpenLayer(crashed, sf.blobs)
	must(t, err)
	if unpublished.Stored() {
		t.Fatal("a staged manifest was loaded")
	}
	if _, err := os.Lstat(filepath.Join(crashed, manifestName+".tmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staged manifest left: %v", err)
	}

	must(t, sf.layer.Store(sf.ids, "tree", sf.blobs))
	data, err := os.ReadFile(filepath.Join(sf.dir, manifestName))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(crashed, manifestName), data, 0o600))
	recovered, err := OpenLayer(crashed, sf.blobs)
	must(t, err)
	if got := layerFiles(t, crashed); !slices.Equal(got, []string{manifestName, "keep.log", "raw/dir/y"}) {
		t.Fatalf("recovered layer directory holds %v", got)
	}
	got := viewOf(t, sf.readOnlyView(t, recovered))
	for p, v := range want {
		if p == "" || p == "keep.log" || strings.HasPrefix(p, "raw") {
			continue // the recovered copy's own raw files and root differ only in their name and host metadata
		}
		if !reflect.DeepEqual(got[p], v) {
			t.Fatalf("%q: recovered %+v, want %+v", p, got[p], v)
		}
	}
}

// TestOpenLayerRejectsBadManifests checks malformed and unknown manifests fail.
func TestOpenLayerRejectsBadManifests(t *testing.T) {
	for name, body := range map[string]string{
		"syntax":         "{",
		"version":        `{"v":2,"e":[{"p":"","k":2,"m":2147484141}]}`,
		"no root":        `{"v":1,"e":[]}`,
		"orphan":         `{"v":1,"e":[{"p":"","k":2,"m":2147484141},{"p":"a/b","k":1,"m":420}]}`,
		"kind mismatch":  `{"v":1,"e":[{"p":"","k":2,"m":2147484141},{"p":"a","k":2,"m":420}]}`,
		"directory blob": `{"v":1,"e":[{"p":"","k":2,"m":2147484141},{"p":"a","k":2,"m":2147484141,"b":"x"}]}`,
		"reserved":       `{"v":1,"e":[{"p":"","k":2,"m":2147484141},{"p":".wh.a","k":1,"m":420}]}`,
	} {
		dir := t.TempDir()
		var data bytes.Buffer
		w, err := flate.NewWriter(&data, flate.DefaultCompression)
		must(t, err)
		_, err = w.Write([]byte(body))
		must(t, err)
		must(t, w.Close())
		must(t, os.WriteFile(filepath.Join(dir, manifestName), data.Bytes(), 0o600))
		if _, err := OpenLayer(dir, memBlobs{}); err == nil {
			t.Fatalf("%s: loaded %s", name, body)
		}
	}
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, manifestName), []byte(`{"v":1}`), 0o600))
	if _, err := OpenLayer(dir, memBlobs{}); err == nil {
		t.Fatal("loaded an uncompressed manifest")
	}
}

// TestFullyStoredLayerIsOneFile checks that a layer with nothing raw left
// becomes its manifest file, and that loading finishes or undoes a collapse a
// crash interrupted.
func TestFullyStoredLayerIsOneFile(t *testing.T) {
	sf := newStoredFixture(t)
	want := viewOf(t, sf.readOnlyView(t, sf.layer))
	ids := maps.Clone(sf.ids)
	ids["keep.log"], ids["raw/dir/y"] = "blob-keep.log", "blob-raw/dir/y"
	sf.blobs["blob-keep.log"], sf.blobs["blob-raw/dir/y"] = []byte("raw"), []byte("raw too")
	must(t, sf.layer.Store(ids, "tree", sf.blobs))
	info, err := os.Lstat(sf.dir)
	must(t, err)
	if !info.Mode().IsRegular() {
		t.Fatalf("fully stored layer is %v", info.Mode())
	}
	assertSameView(t, viewOf(t, sf.readOnlyView(t, sf.layer)), want)
	manifest, err := os.ReadFile(sf.dir)
	must(t, err)

	for name, crash := range map[string]func(dir string){
		"directory removed": func(dir string) {},
		"directory emptied": func(dir string) { must(t, os.Mkdir(dir, 0o755)) },
		"directory kept": func(dir string) {
			must(t, os.Mkdir(dir, 0o755))
			must(t, os.WriteFile(filepath.Join(dir, "keep.log"), []byte("raw"), 0o644))
		},
	} {
		dir := filepath.Join(t.TempDir(), "layer")
		crash(dir)
		must(t, os.WriteFile(dir+collapsing, manifest, 0o600))
		l, err := OpenLayer(dir, sf.blobs)
		must(t, err)
		if _, err := os.Lstat(dir + collapsing); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: staged manifest left: %v", name, err)
		}
		// keep.log is stored too, so the kept directory is pruned and collapses as well.
		if info, err := os.Lstat(dir); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s: layer is %v %v", name, info, err)
		}
		assertSameView(t, viewOf(t, sf.readOnlyView(t, l)), want)
	}
}

// TestStoredLayerCopyUp checks writes, in-memory copy-up, renames and hard
// links over a stored layer.
func TestStoredLayerCopyUp(t *testing.T) {
	sf := newStoredFixture(t)
	runInfo, err := sf.readOnlyView(t, sf.layer).Lstat("src/run.sh")
	must(t, err)
	must(t, sf.layer.Store(sf.ids, "tree", sf.blobs))
	for _, mem := range []*Memory{nil, NewMemory(1<<20, 64<<10, 0)} {
		t.Run(fmt.Sprintf("memory %v", mem != nil), func(t *testing.T) {
			f, err := New(filepath.Join(t.TempDir(), "upper"), []*Layer{sf.layer}, sf.bottom, mem)
			must(t, err)
			write(t, f, "src/run.sh", "echo hi\n", os.O_WRONLY|os.O_APPEND)
			if got := read(t, f, "src/run.sh"); got != "#!/bin/sh\necho hi\n" {
				t.Fatalf("run.sh reads %q", got)
			}
			info, err := f.Lstat("src/run.sh")
			must(t, err)
			if info.Mode() != runInfo.Mode() {
				t.Fatalf("copied-up mode %v, want %v", info.Mode(), runInfo.Mode())
			}
			must(t, f.Chtimes("deep/er/x", past, past))
			if info, err := f.Lstat("deep/er/x"); err != nil || !info.ModTime().Equal(past) || read(t, f, "deep/er/x") != "deep" {
				t.Fatalf("deep/er/x after chtimes: %v %v", info, err)
			}
			must(t, f.Rename("src/link", "src/link2"))
			if target, err := f.Readlink("src/link2"); err != nil || target != "a.txt" {
				t.Fatalf("renamed link points to %q: %v", target, err)
			}
			must(t, f.Rename("src/hard", "src/a.txt"))
			assertNames(t, f, "src", "a.txt", "base", "hard", "link2", "run.sh")
			if got := read(t, f, "src/hard"); got != "alpha" {
				t.Fatalf("hard link reads %q", got)
			}
		})
	}
}
