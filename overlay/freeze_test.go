package overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFreezeMatchesDiskIndex compares the consumed cache with a fresh layer walk.
func TestFreezeMatchesDiskIndex(t *testing.T) {
	for _, seed := range []int64{1, 17, 83} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			f := stackFixture(t)
			must(t, f.MkdirAll("random", 0o755))
			rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)^0x9e3779b97f4a7c15))
			for i := range 250 {
				p := fmt.Sprintf("random/n%d", rng.IntN(20))
				var err error
				switch rng.IntN(8) {
				case 0, 1:
					write(t, f, p, fmt.Sprint(i), os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
				case 2:
					err = f.Remove(p)
				case 3:
					err = f.Rename(p, p+"m")
				case 4:
					err = f.Link(p, p+"l")
				case 5:
					err = f.Chmod(p, 0o600|os.FileMode(i&1)*0o111)
				case 6:
					err = f.Chtimes(p, past, past.Add(time.Duration(i)*time.Second))
				case 7:
					err = f.Remove(p + "l")
				}
				if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrExist) {
					t.Fatal(err)
				}
			}
			must(t, f.Remove("d/top"))
			must(t, f.MkdirAll("deleted", 0o755))
			must(t, f.Symlink("../d/base", "random/link"))
			must(t, f.Rename("random", "renamed"))
			assertFrozenIndexMatchesDisk(t, freezeForTest(t, f))
		})
	}
}

// TestModesSurviveSpillReopenAndFreeze keeps chmod and create modes, including
// setuid and sticky bits, through copy-up into memory, a spill to disk, hard
// links, renames, reopening the upper and freezing it: on hosts whose files
// cannot hold Unix modes (Windows) as on those that can.
func TestModesSurviveSpillReopenAndFreeze(t *testing.T) {
	stack := stackFixture(t)
	f, err := New(t.TempDir(), stack.Stack(), stack.bottom, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	must(t, f.Chmod("d/low", 0o750|os.ModeSetuid))
	must(t, f.Flush())
	must(t, f.Link("d/low", "linked"))
	must(t, f.Rename("d/low", "d/moved"))
	must(t, f.MkdirAll("dir", 0o755))
	must(t, f.Chmod("dir", 0o750|os.ModeSticky))
	h, err := f.OpenFile("dir/new", os.O_CREATE|os.O_WRONLY, 0o600)
	must(t, err)
	must(t, h.Close())
	must(t, f.Flush())
	want := map[string]os.FileMode{
		"d/moved": 0o750 | os.ModeSetuid,
		"linked":  0o750 | os.ModeSetuid,
		"dir":     os.ModeDir | os.ModeSticky | 0o750,
		"dir/new": 0o600,
	}
	check := func(stage string, mode func(string) os.FileMode) {
		t.Helper()
		for p, m := range want {
			if got := mode(p); got != m {
				t.Errorf("%s: %s mode = %v, want %v", stage, p, got, m)
			}
		}
	}
	lstat := func(f *FS) func(string) os.FileMode {
		return func(p string) os.FileMode {
			info, err := f.Lstat(p)
			must(t, err)
			return info.Mode()
		}
	}
	check("live", lstat(f))
	reopened, err := New(f.Upper(), stack.Stack(), stack.bottom, nil)
	must(t, err)
	check("reopened", lstat(reopened))
	frozen := freezeForTest(t, reopened)
	check("frozen", func(p string) os.FileMode {
		e, ok := frozen.Get(p)
		if !ok {
			t.Fatalf("frozen layer lacks %s", p)
		}
		return e.Info.Mode()
	})
}

// TestFreezeTracksHardlinksAndOpenRenames exercises metadata changes through aliases.
func TestFreezeTracksHardlinksAndOpenRenames(t *testing.T) {
	f := stackFixture(t)
	write(t, f, "live", "initial", os.O_CREATE|os.O_RDWR)
	must(t, f.Link("live", "alias"))
	h, err := f.OpenFile("live", os.O_RDWR, 0)
	must(t, err)
	must(t, f.Rename("live", "moved"))
	_, err = h.Write([]byte("updated contents"))
	must(t, err)
	must(t, h.Truncate(12))
	must(t, h.Close())
	must(t, f.Chmod("alias", 0o750))
	must(t, f.Chtimes("moved", past, past.Add(time.Hour)))
	must(t, f.Link("alias", "removed"))
	must(t, f.Remove("removed"))
	must(t, f.Rename("alias", "moved"))
	// Renaming two names of one inode is a no-op, retaining both names.
	if _, err := f.Lstat("alias"); err != nil {
		t.Fatal(err)
	}
	write(t, f, "victim", "old", os.O_CREATE|os.O_RDWR)
	must(t, f.Link("victim", "surviving-victim"))
	must(t, f.Rename("moved", "victim"))
	must(t, f.Symlink("victim", "metadata-link"))
	must(t, f.Chtimes("metadata-link", past, past))
	readOnly, err := f.Open("victim")
	must(t, err)
	if descriptor, ok := readOnly.(interface{ Chmod(os.FileMode) error }); ok {
		must(t, descriptor.Chmod(0o640))
	}
	must(t, readOnly.Close())
	if got := read(t, f, "alias"); got != "updated cont" {
		t.Fatalf("alias data = %q", got)
	}
	assertFrozenIndexMatchesDisk(t, freezeForTest(t, f))
}

// TestFreezeTracksExistingHardlinks includes aliases loaded when reopening an upper.
func TestFreezeTracksExistingHardlinks(t *testing.T) {
	upper := tree(t, map[string]string{"original": "before", "dir/child": "data", opaqueName: ""})
	must(t, os.Link(filepath.Join(upper, "original"), filepath.Join(upper, "alias")))
	f, err := New(upper, nil, nil, nil)
	must(t, err)
	write(t, f, "alias", "after mutation", os.O_WRONLY|os.O_TRUNC)
	must(t, f.Chmod("original", 0o750))
	must(t, f.Remove("alias"))
	assertFrozenIndexMatchesDisk(t, freezeForTest(t, f))
}

// TestFreezeReadOnlyAndFailureLeavesCache rejects invalid freezes without consuming data.
func TestFreezeReadOnlyAndFailureLeavesCache(t *testing.T) {
	ro, err := New("", nil, nil, nil)
	must(t, err)
	if _, err := ro.Freeze(t.TempDir()); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("read-only freeze: %v", err)
	}
	f := stackFixture(t)
	write(t, f, "kept", "value", os.O_CREATE|os.O_WRONLY)
	if _, err := f.Freeze(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing destination: %v", err)
	}
	if got := read(t, f, "kept"); got != "value" {
		t.Fatal(got)
	}
	assertFrozenIndexMatchesDisk(t, freezeForTest(t, f))
}

// freezeForTest performs the caller-owned rename and work-directory removal.
func freezeForTest(t *testing.T, f *FS) *Layer {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "frozen")
	must(t, os.Rename(f.Upper(), dir))
	must(t, os.RemoveAll(filepath.Join(dir, workName)))
	l, err := f.Freeze(dir)
	must(t, err)
	if l.Dir() != dir {
		t.Fatalf("frozen dir = %q", l.Dir())
	}
	return l
}

// assertFrozenIndexMatchesDisk compares entries, stable stat fields and child indexes.
func assertFrozenIndexMatchesDisk(t *testing.T, frozen *Layer) {
	t.Helper()
	disk, err := IndexLayer(frozen.Dir())
	must(t, err)
	var gotPaths, wantPaths []string
	must(t, frozen.Walk(func(p string, e Entry) error { gotPaths = append(gotPaths, p); return nil }))
	must(t, disk.Walk(func(p string, e Entry) error { wantPaths = append(wantPaths, p); return nil }))
	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("frozen paths = %v; disk paths = %v", gotPaths, wantPaths)
	}
	for _, p := range wantPaths {
		got, ok := frozen.Get(p)
		want, _ := disk.Get(p)
		if !ok || got.Kind != want.Kind || got.Opaque != want.Opaque {
			t.Fatalf("entry %q: %+v != %+v", p, got, want)
		}
		if (got.Info == nil) != (want.Info == nil) {
			t.Fatalf("metadata presence differs at %q", p)
		}
		if want.Info != nil {
			if !reflect.DeepEqual(stableSystemInfo(got.Info), stableSystemInfo(want.Info)) {
				t.Fatalf("system metadata differs at %q: %v != %v", p, stableSystemInfo(got.Info), stableSystemInfo(want.Info))
			}
			if got.Info.Name() != want.Info.Name() || got.Info.Size() != want.Info.Size() || got.Info.Mode() != want.Info.Mode() || !got.Info.ModTime().Equal(want.Info.ModTime()) || got.Info.IsDir() != want.Info.IsDir() || !sameFile(got.Info, want.Info) {
				t.Fatalf("metadata %q: got %s %d %s %v; want %s %d %s %v", p, got.Info.Name(), got.Info.Size(), got.Info.Mode(), got.Info.ModTime(), want.Info.Name(), want.Info.Size(), want.Info.Mode(), want.Info.ModTime())
			}
		}
		if !reflect.DeepEqual(childKinds(frozen.Children(p)), childKinds(disk.Children(p))) {
			t.Fatalf("child entries differ at %q", p)
		}
	}
}

// stableSystemInfo excludes access times, which remain cached snapshot metadata.
func stableSystemInfo(info os.FileInfo) any {
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return info.Sys()
	}
	out := reflect.New(value.Type()).Elem()
	out.Set(value)
	for i := range value.NumField() {
		name := value.Type().Field(i).Name
		if strings.HasPrefix(name, "Atim") || name == "LastAccessTime" {
			out.Field(i).SetZero()
		}
	}
	return out.Interface()
}

// childKinds compares direct entry structure without depending on stat pointers.
func childKinds(children map[string]Entry) map[string]Entry {
	for name, e := range children {
		e.Info = nil
		children[name] = e
	}
	return children
}

// TestLayerGitEntries indexes every entry named .git as the upper changes, so a
// frozen layer lists them as an index of its own directory would, without a walk.
func TestLayerGitEntries(t *testing.T) {
	f := setup(t, map[string]string{"a/.git": "gitdir: x\n", "b/c/keep.txt": "k", "gone/.git": "gitdir: y\n"})
	write(t, f, "b/c/.git", "gitdir: z\n", os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	write(t, f, "a/.git", "gitdir: w\n", os.O_WRONLY|os.O_TRUNC)
	must(t, f.MkdirAll("d/.git/refs", 0o755))
	must(t, f.MkdirAll("e", 0o755))
	write(t, f, "e/.git", "gitdir: v\n", os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	must(t, f.Remove("e/.git"))
	must(t, f.Remove("gone/.git"))
	must(t, f.Flush())
	l := freezeForTest(t, f)
	disk, err := IndexLayer(l.Dir())
	must(t, err)
	want := []string{"a/.git", "b/c/.git", "d/.git", "gone/.git"}
	for name, got := range map[string][]string{"frozen": l.GitEntries(), "indexed": disk.GitEntries()} {
		if slices.Sort(got); !slices.Equal(got, want) {
			t.Errorf("%s layer .git entries = %v, want %v", name, got, want)
		}
	}
	if e, _ := l.Get("gone/.git"); e.Kind != KindWhiteout {
		t.Errorf("gone/.git = %+v, want a whiteout", e)
	}
}
