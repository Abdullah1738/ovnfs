package overlay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
)

// TestMemoryMatchesDisk runs one random operation sequence on a memory upper
// whose tiny budget forces spills by size and by eviction, and on a disk upper
// over the same layers. The merged views, errors and open-handle contents must
// agree throughout, and after Flush the memory upper must equal the disk one.
func TestMemoryMatchesDisk(t *testing.T) {
	disk := stackFixture(t)
	budget := NewMemory(6<<10, 2<<10, 0)
	mem, err := New(t.TempDir(), disk.Stack(), disk.bottom, budget)
	must(t, err)
	both := [2]*FS{disk, mem}
	files := []string{"d/low", "d/top", "d/base", "d/replace", "d/sub/low", "d/new", "d/sub/new", "x/a", "x/b"}
	dirs := []string{"d/sub", "x", "x/y", "d/moved"}
	modes := []os.FileMode{0o644, 0o600, 0o444, 0o755}
	var held [][2]billy.File
	rng := rand.New(rand.NewPCG(5, 23))
	for step := range 1500 {
		p, q := files[rng.IntN(len(files))], files[rng.IntN(len(files))]
		dir := dirs[rng.IntN(len(dirs))]
		size, off := rng.IntN(1200), int64(rng.IntN(1200))
		if rng.IntN(6) == 0 {
			size, off = 1<<10+rng.IntN(2<<10), int64(rng.IntN(3<<10)) // outgrows the per-file limit
		}
		data := bytes.Repeat([]byte{byte('a' + step%26)}, size)
		mode := modes[rng.IntN(len(modes))]
		if info, err := disk.Lstat(p); err == nil && info.IsDir() {
			mode = 0o755 // directories stay searchable so the views remain walkable
		}
		stamp := past.Add(time.Duration(step) * time.Second)
		op := rng.IntN(15)
		pick := -1
		if len(held) > 0 {
			pick = rng.IntN(len(held))
		}
		var errs [2]error
		var opened [2]billy.File
		var got [2][]byte
		for k, f := range both {
			errs[k] = func() error {
				switch op {
				case 0:
					return writeFile(f, p, data, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
				case 1:
					return writeFile(f, p, data, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
				case 2:
					h, err := f.OpenFile(p, os.O_RDWR, 0)
					if err != nil {
						return err
					}
					_, err = h.(io.WriterAt).WriteAt(data, off)
					return errors.Join(err, h.Close())
				case 3:
					h, err := f.OpenFile(p, os.O_RDWR, 0)
					if err != nil {
						return err
					}
					return errors.Join(h.Truncate(off), h.Close())
				case 4:
					return f.Remove(p)
				case 5:
					return f.Rename(p, q)
				case 6:
					return f.Rename(dir, p)
				case 7:
					return f.Chmod(p, mode)
				case 8:
					return f.Chtimes(p, stamp, stamp)
				case 9:
					return f.Link(p, q)
				case 10:
					return f.MkdirAll(dir, 0o755)
				case 11:
					return f.Rename(p, path.Join(dir, path.Base(p)))
				case 12:
					h, err := f.OpenFile(p, os.O_RDWR, 0)
					opened[k] = h
					return err
				case 13:
					if pick < 0 {
						return nil
					}
					h := held[pick][k]
					return errors.Join(h.Lock(), h.Unlock())
				default:
					if pick < 0 {
						return nil
					}
					h := held[pick][k]
					if _, err := h.(io.WriterAt).WriteAt(data, off); err != nil {
						return err
					}
					info, err := h.(interface{ Stat() (os.FileInfo, error) }).Stat()
					if err != nil {
						return err
					}
					if _, err := h.Seek(0, io.SeekStart); err != nil {
						return err
					}
					b, err := io.ReadAll(h)
					if err != nil {
						return err
					}
					tail := make([]byte, 512)
					n, err := h.ReadAt(tail, off/2)
					if err != nil && err != io.EOF {
						return err
					}
					got[k] = fmt.Appendf(b, " size=%d tail=%q", info.Size(), tail[:n])
					return nil
				}
			}()
		}
		if errClass(errs[0]) != errClass(errs[1]) {
			t.Fatalf("step %d op %d (%s, %s, %s): disk %v, memory %v", step, op, p, q, dir, errs[0], errs[1])
		}
		if opened[0] != nil {
			held = append(held, opened)
		}
		if !bytes.Equal(got[0], got[1]) {
			t.Fatalf("step %d: held handle reads %q on disk, %q in memory", step, got[0], got[1])
		}
		if a, b := mergedTree(t, disk), mergedTree(t, mem); !maps.Equal(a, b) {
			t.Fatalf("step %d op %d (%s, %s, %s): views differ\ndisk   %v\nmemory %v", step, op, p, q, dir, a, b)
		}
		if used, limit := budget.Usage(); used > limit {
			t.Fatalf("step %d: %d bytes in memory exceed the %d budget", step, used, limit)
		}
	}
	for _, h := range held {
		must(t, h[0].Close())
		must(t, h[1].Close())
	}
	must(t, mem.Flush())
	assertIndexMatchesDisk(t, mem)
	if a, b := upperTree(t, disk.upper), upperTree(t, mem.upper); !maps.Equal(a, b) {
		t.Fatalf("flushed upper differs from the disk upper\ndisk   %v\nmemory %v", a, b)
	}
	if used, _ := budget.Usage(); used != 0 {
		t.Fatalf("flush left %d bytes charged", used)
	}
}

// TestMemoryEvictsAcrossOverlays spills another overlay's least recently used
// files when the shared budget is full, keeping their contents.
func TestMemoryEvictsAcrossOverlays(t *testing.T) {
	budget := NewMemory(16<<10, 4<<10, 0)
	a, err := New(t.TempDir(), nil, nil, budget)
	must(t, err)
	b, err := New(t.TempDir(), nil, nil, budget)
	must(t, err)
	content := func(fsName string, i int) string { return strings.Repeat(fsName+fmt.Sprint(i), 300) }
	for i := range 6 {
		write(t, a, fmt.Sprintf("f%d", i), content("a", i), os.O_CREATE|os.O_WRONLY)
	}
	for i := range 6 {
		write(t, b, fmt.Sprintf("f%d", i), content("b", i), os.O_CREATE|os.O_WRONLY)
	}
	if used, limit := budget.Usage(); used > limit {
		t.Fatalf("%d bytes in memory exceed the %d budget", used, limit)
	}
	if _, err := os.Stat(filepath.Join(a.upper, "f0")); err != nil {
		t.Fatalf("the coldest file of the other overlay did not spill: %v", err)
	}
	if !b.Unflushed() {
		t.Fatal("the newest files left memory")
	}
	for i := range 6 {
		for name, f := range map[string]*FS{"a": a, "b": b} {
			if got := read(t, f, fmt.Sprintf("f%d", i)); got != content(name, i) {
				t.Fatalf("%s/f%d = %.12q…", name, i, got)
			}
		}
	}
}

// TestMemoryWritesBackIdleFiles moves files left untouched for the idle age
// into the upper in the background, while an open handle keeps working and
// files deleted before then never reach the disk.
func TestMemoryWritesBackIdleFiles(t *testing.T) {
	budget := NewMemory(1<<20, 64<<10, 20*time.Millisecond)
	f, err := New(t.TempDir(), nil, nil, budget)
	must(t, err)
	h, err := f.OpenFile("open", os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	_, err = h.Write([]byte("before "))
	must(t, err)
	write(t, f, "temp", "short-lived", os.O_CREATE|os.O_WRONLY)
	must(t, f.Remove("temp"))
	deadline := time.Now().Add(5 * time.Second)
	for f.Unflushed() {
		if time.Now().After(deadline) {
			t.Fatal("idle file still in memory after 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err = h.Write([]byte("after"))
	must(t, err)
	must(t, h.Close())
	if data, err := os.ReadFile(filepath.Join(f.upper, "open")); err != nil || string(data) != "before after" {
		t.Fatalf("upper holds %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(f.upper, "temp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file removed while in memory reached the upper: %v", err)
	}
	if used, _ := budget.Usage(); used != 0 {
		t.Fatalf("writeback left %d bytes charged", used)
	}
}

// TestMemoryNameChangesStampParent keeps NFS directory caches valid: creating,
// renaming or removing a memory file advances its directory's mtime as a host
// operation would, while a copy-up into memory leaves it alone.
func TestMemoryNameChangesStampParent(t *testing.T) {
	lower := tree(t, map[string]string{"d/lower": "data"})
	f, err := New(filepath.Join(t.TempDir(), "u"), nil, dirBottom{lower}, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	stamped := func(change func() error) bool {
		t.Helper()
		must(t, f.Chtimes("d", past, past))
		must(t, change())
		info, err := f.Lstat("d")
		must(t, err)
		return info.ModTime().After(past)
	}
	if stamped(func() error { return writeFile(f, "d/lower", []byte("X"), os.O_RDWR) }) {
		t.Fatal("copy-up changed the directory mtime")
	}
	if !f.Unflushed() {
		t.Fatal("copy-up did not stay in memory")
	}
	if !stamped(func() error { return writeFile(f, "d/new", []byte("n"), os.O_CREATE|os.O_WRONLY) }) {
		t.Fatal("create left the directory mtime unchanged")
	}
	if !stamped(func() error { return f.Rename("d/new", "d/renamed") }) {
		t.Fatal("rename left the directory mtime unchanged")
	}
	if !stamped(func() error { return f.Remove("d/renamed") }) {
		t.Fatal("remove left the directory mtime unchanged")
	}
}

// TestMemoryFilesShowTheirDiskOwner reports a memory file's owner as a host
// create in its directory would set it, so moving the file to disk changes
// no attribute a client caches.
func TestMemoryFilesShowTheirDiskOwner(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("numeric owners")
	}
	groups, err := os.Getgroups()
	must(t, err)
	alt := -1
	for _, g := range groups {
		if g != os.Getegid() {
			alt = g
			break
		}
	}
	if alt < 0 {
		t.Skip("needs a supplementary group")
	}
	upper := t.TempDir()
	// The directory hands its group to new files: always on darwin, setgid on Linux.
	must(t, os.Chown(upper, -1, alt))
	must(t, os.Chmod(upper, 0o755|os.ModeSetgid))
	f, err := New(upper, nil, nil, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	write(t, f, "new", "data", os.O_CREATE|os.O_WRONLY)
	info, err := f.Lstat("new")
	must(t, err)
	want := fmt.Sprintf("%d:%d", os.Geteuid(), alt)
	if got := owner(info); got != want {
		t.Fatalf("memory file owner %s, want %s", got, want)
	}
	must(t, f.Flush())
	info, err = f.Lstat("new")
	must(t, err)
	if got := owner(info); got != want {
		t.Fatalf("owner after moving to disk %s, want %s", got, want)
	}
}

// writeFile writes data through a fresh descriptor opened with flag.
func writeFile(f *FS, p string, data []byte, flag int) error {
	h, err := f.OpenFile(p, flag, 0o666)
	if err != nil {
		return err
	}
	_, err = h.Write(data)
	return errors.Join(err, h.Close())
}

// errClass reduces an error to what callers can observe: its errno or sentinel.
func errClass(err error) string {
	var errno syscall.Errno
	switch {
	case err == nil:
		return ""
	case errors.As(err, &errno):
		return errno.Error()
	case errors.Is(err, fs.ErrNotExist):
		return "not exist"
	case errors.Is(err, fs.ErrExist):
		return "exist"
	}
	return err.Error()
}

// mergedTree describes every visible entry: kind, permissions, size and content.
func mergedTree(t *testing.T, f *FS) map[string]string {
	t.Helper()
	out := make(map[string]string)
	var walk func(string)
	walk = func(dir string) {
		infos, err := f.ReadDir(dir)
		must(t, err)
		for _, info := range infos {
			p := path.Join(dir, info.Name())
			if info.IsDir() {
				out[p] = fmt.Sprintf("dir %v", info.Mode().Perm())
				walk(p)
				continue
			}
			lst, err := f.Lstat(p)
			must(t, err)
			if lst.Size() != info.Size() || lst.Mode() != info.Mode() {
				t.Fatalf("%s: listing %v/%d disagrees with lstat %v/%d", p, info.Mode(), info.Size(), lst.Mode(), lst.Size())
			}
			content := "<unreadable>"
			if info.Mode().Perm()&0o400 != 0 {
				content = read(t, f, p)
			}
			out[p] = fmt.Sprintf("file %v %d %s %q", info.Mode(), info.Size(), owner(info), content)
		}
	}
	walk("")
	return out
}

// upperTree describes a host upper (markers included, staging excluded) by
// kind, permissions and content.
func upperTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	must(t, filepath.Walk(dir, func(real string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, real)
		if err != nil {
			return err
		}
		switch {
		case rel == workName:
			return filepath.SkipDir
		case info.IsDir():
			out[rel] = fmt.Sprintf("dir %v", info.Mode().Perm())
		default:
			data, err := os.ReadFile(real)
			if err != nil && !errors.Is(err, fs.ErrPermission) {
				return err
			}
			out[rel] = fmt.Sprintf("file %v %q", info.Mode(), data)
		}
		return nil
	}))
	return out
}

// TestMemorySyncReachesDisk makes fsync mean what it does on any filesystem:
// a synced memory file is in its upper on disk, with the bytes written so far,
// so a daemon killed right after loses none of it. Later writes through the
// same handle go to that host file.
func TestMemorySyncReachesDisk(t *testing.T) {
	upper := filepath.Join(t.TempDir(), "u")
	f, err := New(upper, nil, dirBottom{tree(t, map[string]string{"d/lower": "data"})}, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	for _, name := range []string{"d/new", "d/lower"} {
		h, err := f.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
		must(t, err)
		_, err = h.Write([]byte("synced"))
		must(t, err)
		if _, err := os.Stat(filepath.Join(upper, name)); err == nil {
			t.Fatalf("%s reached the upper before sync; the test needs it in memory", name)
		}
		must(t, h.(interface{ Sync() error }).Sync())
		if b, err := os.ReadFile(filepath.Join(upper, name)); err != nil || string(b) != "synced" {
			t.Fatalf("%s after sync: upper holds %q, %v", name, b, err)
		}
		_, err = h.Write([]byte(" more"))
		must(t, err)
		must(t, h.Close())
		if b, err := os.ReadFile(filepath.Join(upper, name)); err != nil || string(b) != "synced more" {
			t.Fatalf("%s after a later write: upper holds %q, %v", name, b, err)
		}
	}
	if f.Unflushed() {
		t.Fatal("a synced file stayed in memory")
	}
}
