//go:build (darwin && cgo) || linux || windows

package watch

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder collects the paths a Watcher reports.
type recorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *recorder) fn(paths []string) {
	r.mu.Lock()
	r.paths = append(r.paths, paths...)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.paths)
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.paths = nil
	r.mu.Unlock()
}

// waitFor fails t unless p is reported within a few seconds.
func (r *recorder) waitFor(t *testing.T, p string) {
	t.Helper()
	if !soon(func() bool { return slices.Contains(r.snapshot(), p) }) {
		t.Fatalf("%s not reported; got %q", p, r.snapshot())
	}
}

// soon reports whether cond holds within a few seconds.
func soon(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func start(t *testing.T, fn func([]string)) *Watcher {
	t.Helper()
	w, err := New(fn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func add(t *testing.T, w *Watcher, dir string) {
	t.Helper()
	if err := w.Add(dir); err != nil {
		t.Fatal(err)
	}
}

// tempDir returns a directory whose spelling differs from its real path where
// the platform allows, so the mapping back from canonical paths is exercised.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		return dir // macOS: /var -> /private/var
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		if runtime.GOOS == "windows" {
			return dir // symlinks need a privilege there
		}
		t.Fatal(err)
	}
	return link
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func rename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func TestReportsChangesUnderAddSpelling(t *testing.T) {
	var r recorder
	w := start(t, r.fn)
	dir := tempDir(t)
	add(t, w, dir)
	f := filepath.Join(dir, "f")

	write(t, f, "a")
	r.waitFor(t, f)
	r.reset()

	fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("b"); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	r.waitFor(t, f)
	r.reset()

	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, f)

	// A rename reports both names.
	g := filepath.Join(dir, "g")
	write(t, filepath.Join(dir, "h"), "")
	rename(t, filepath.Join(dir, "h"), g)
	r.waitFor(t, filepath.Join(dir, "h"))
	r.waitFor(t, g)

	for _, p := range r.snapshot() {
		if p != dir && !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Errorf("reported %s outside %s", p, dir)
		}
	}
}

// TestSyncReportsEarlierChanges finds every change made before Sync already
// reported when it returns, with no waiting: new files, rewritten files, and a
// file written through a handle once the handle closed (Windows reports such
// writes only then).
func TestSyncReportsEarlierChanges(t *testing.T) {
	var r recorder
	w := start(t, r.fn)
	dir := tempDir(t)
	add(t, w, dir)
	kept := filepath.Join(dir, "kept")
	write(t, kept, "0")
	for i := range 100 {
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
		r.reset()
		fresh := filepath.Join(dir, "new"+strconv.Itoa(i))
		write(t, fresh, "x")
		write(t, kept, strings.Repeat("y", i+2))
		open, err := os.OpenFile(filepath.Join(dir, "open"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, err = open.Write([]byte("z"))
		if closeErr := open.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
		got := r.snapshot()
		for _, p := range []string{fresh, kept, open.Name()} {
			if !slices.Contains(got, p) {
				t.Fatalf("round %d: %s not reported by Sync; got %q", i, p, got)
			}
		}
	}
}

func TestReportsUnderCaseInsensitiveSpelling(t *testing.T) {
	var r recorder
	w := start(t, r.fn)
	base := tempDir(t)
	mkdir(t, filepath.Join(base, "Src"))
	dir := filepath.Join(base, "src")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("temp volume is case-sensitive")
	}
	add(t, w, dir)
	write(t, filepath.Join(dir, "F"), "")
	r.waitFor(t, filepath.Join(dir, "F"))
}

func TestReportsNestedPaths(t *testing.T) {
	var r recorder
	w := start(t, r.fn)
	dir := tempDir(t)
	add(t, w, dir)
	sub := filepath.Join(dir, "a", "b", "c")
	mkdir(t, sub)
	write(t, filepath.Join(sub, "f"), "x")
	r.waitFor(t, filepath.Join(dir, "a"))
	r.waitFor(t, sub)
	r.waitFor(t, filepath.Join(sub, "f"))
}

func TestReportsInsideMovedDirs(t *testing.T) {
	var r recorder
	w := start(t, r.fn)
	dir := tempDir(t)
	add(t, w, dir)

	// Renamed within the directory.
	mkdir(t, filepath.Join(dir, "a", "deep"))
	r.waitFor(t, filepath.Join(dir, "a", "deep"))
	rename(t, filepath.Join(dir, "a"), filepath.Join(dir, "b"))
	r.waitFor(t, filepath.Join(dir, "b"))
	write(t, filepath.Join(dir, "b", "deep", "f"), "")
	r.waitFor(t, filepath.Join(dir, "b", "deep", "f"))

	// Moved in from elsewhere.
	outside := t.TempDir()
	mkdir(t, filepath.Join(outside, "o", "p"))
	rename(t, filepath.Join(outside, "o"), filepath.Join(dir, "o"))
	r.waitFor(t, filepath.Join(dir, "o"))
	write(t, filepath.Join(dir, "o", "p", "g"), "")
	r.waitFor(t, filepath.Join(dir, "o", "p", "g"))
}

func TestRemoveStopsReportsForThatDirOnly(t *testing.T) {
	var r recorder
	w := start(t, r.fn)
	a, b := tempDir(t), tempDir(t)
	add(t, w, a)
	add(t, w, b)
	write(t, filepath.Join(a, "1"), "")
	write(t, filepath.Join(b, "1"), "")
	r.waitFor(t, filepath.Join(a, "1"))
	r.waitFor(t, filepath.Join(b, "1"))

	w.Remove(a)
	r.reset()
	write(t, filepath.Join(a, "2"), "")
	write(t, filepath.Join(b, "2"), "")
	r.waitFor(t, filepath.Join(b, "2"))
	// Give a/2 the time b/2 took, in case the backend reports out of order.
	time.Sleep(100 * time.Millisecond)
	for _, p := range r.snapshot() {
		if strings.HasPrefix(p, a) {
			t.Errorf("reported %s after Remove(%s)", p, a)
		}
	}
}

// TestNestedDirsSurviveRemovals keeps reporting below a dir while another
// added dir around it comes and goes.
func TestNestedDirsSurviveRemovals(t *testing.T) {
	var r recorder
	w := start(t, r.fn)
	outer := tempDir(t)
	inner := filepath.Join(outer, "in")
	mkdir(t, filepath.Join(inner, "sub"))

	add(t, w, inner)
	add(t, w, outer)
	write(t, filepath.Join(inner, "sub", "1"), "")
	r.waitFor(t, filepath.Join(inner, "sub", "1"))
	r.waitFor(t, filepath.Join(outer, "in", "sub", "1"))

	w.Remove(outer)
	if w.Covers(outer) || !w.Covers(inner) {
		t.Fatalf("after Remove(outer): Covers(outer) = %v, Covers(inner) = %v", w.Covers(outer), w.Covers(inner))
	}
	write(t, filepath.Join(inner, "sub", "2"), "")
	r.waitFor(t, filepath.Join(inner, "sub", "2"))

	add(t, w, outer)
	w.Remove(inner)
	write(t, filepath.Join(inner, "sub", "3"), "")
	r.waitFor(t, filepath.Join(outer, "in", "sub", "3"))
}

// TestRecreatedDirIsWatchedAgain reports a deleted dir wholly changed and,
// once it exists again, what changes in it.
func TestRecreatedDirIsWatchedAgain(t *testing.T) {
	var r recorder
	w := start(t, r.fn)
	dir := filepath.Join(tempDir(t), "d")
	mkdir(t, dir)
	add(t, w, dir)
	if !w.Covers(dir) {
		t.Fatal("added dir not covered")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, dir)
	if runtime.GOOS != "darwin" && w.Covers(dir) {
		t.Fatal("deleted dir still covered")
	}
	mkdir(t, dir)
	if !soon(func() bool { return w.Covers(dir) }) {
		t.Fatal("recreated dir never covered")
	}
	write(t, filepath.Join(dir, "f"), "")
	r.waitFor(t, filepath.Join(dir, "f"))
}

func TestChangingDirsKeepsEventsForStayingDir(t *testing.T) {
	var r recorder
	w := start(t, r.fn)
	stay := tempDir(t)
	add(t, w, stay)
	others := []string{tempDir(t), tempDir(t)}
	var files []string
	for i := range 12 {
		f := filepath.Join(stay, strconv.Itoa(i))
		write(t, f, "")
		files = append(files, f)
		if o := others[i%2]; i%4 < 2 {
			add(t, w, o)
		} else {
			w.Remove(o)
		}
	}
	for _, f := range files {
		r.waitFor(t, f)
	}
	if !w.Covers(stay) {
		t.Error("staying dir not covered")
	}
}

func TestCloseWaitsForCallback(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls sync.Mutex
	n := 0
	w := start(t, func([]string) {
		calls.Lock()
		n++
		calls.Unlock()
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	dir := tempDir(t)
	add(t, w, dir)
	write(t, filepath.Join(dir, "f"), "")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no callback")
	}

	closed := make(chan struct{})
	go func() {
		w.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a callback was running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the callback finished")
	}

	calls.Lock()
	before := n
	calls.Unlock()
	write(t, filepath.Join(dir, "g"), "")
	time.Sleep(200 * time.Millisecond)
	calls.Lock()
	defer calls.Unlock()
	if n != before {
		t.Errorf("fn called %d times after Close", n-before)
	}
}
