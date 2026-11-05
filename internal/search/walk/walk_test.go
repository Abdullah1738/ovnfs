package walk

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
)

// makeTree creates files (content after "=") and directories (trailing "/")
// below a fresh temporary root.
func makeTree(t *testing.T, specs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, spec := range specs {
		name, content, _ := strings.Cut(spec, "=")
		p := filepath.Join(root, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// isolateGit points git's global and system config lookups at an empty
// directory, returning the XDG config home for tests that need excludes.
func isolateGit(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "none"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(home, "none"))
	return filepath.Join(home, ".config")
}

// paths collects the walk-relative paths opts admits under root, sorted,
// checking that Walk and both Collect orders agree.
func paths(t *testing.T, root string, opts Options) []string {
	t.Helper()
	opts.Sorted = true
	sorted, err := Collect(t.Context(), root, opts)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range sorted {
		got = append(got, e.Path)
	}
	if !slices.IsSorted(got) {
		t.Errorf("sorted Collect out of order: %q", got)
	}
	opts.Sorted = false
	unordered, err := Collect(t.Context(), root, opts)
	if err != nil {
		t.Fatal(err)
	}
	var streamed []string
	var mu atomic.Int32
	err = Walk(t.Context(), root, opts, func(_ *Worker, e *Entry) Action {
		for !mu.CompareAndSwap(0, 1) {
		}
		streamed = append(streamed, strings.Clone(e.Path))
		mu.Store(0)
		return Continue
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range [][]string{entryPaths(unordered), streamed} {
		slices.Sort(other)
		if !slices.Equal(other, got) {
			t.Errorf("walk modes disagree:\n%q\n%q", got, other)
		}
	}
	return got
}

func entryPaths(entries []Entry) []string {
	out := make([]string, len(entries))
	for i := range entries {
		out[i] = entries[i].Path
	}
	return out
}

func wantPaths(t *testing.T, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestPruningOptions(t *testing.T) {
	isolateGit(t)
	root := makeTree(t, "a.txt", ".env", ".hidden/x.txt", ".git/config",
		"node_modules/m/i.js", "src/main.go", "src/node_modules", "src/.git")
	wantPaths(t, paths(t, root, Options{}),
		"a.txt", "node_modules", "node_modules/m", "node_modules/m/i.js", "src", "src/main.go", "src/node_modules")
	wantPaths(t, paths(t, root, Options{Hidden: true, SkipGit: true, SkipNodeModules: true}),
		".env", ".hidden", ".hidden/x.txt", "a.txt", "src", "src/main.go")
	wantPaths(t, paths(t, root, Options{Hidden: true, MaxDepth: 1}),
		".env", ".git", ".hidden", "a.txt", "node_modules", "src")
}

func TestIgnorePrecedence(t *testing.T) {
	isolateGit(t)
	root := makeTree(t,
		".git/info/exclude=excluded.txt\n",
		".gitignore=*.log\n!keep.log\nbuild/\n",
		".ignore=!forced.log\n",
		"keep.log", "drop.log", "forced.log", "excluded.txt", "build/out.txt", "plain/build",
		"sub/.gitignore=!drop.log\n", "sub/drop.log", "sub/x.log")
	wantPaths(t, paths(t, root, Options{Gitignore: true}),
		"forced.log", "keep.log", "plain", "plain/build", "sub", "sub/drop.log")
}

func TestRepositoryScopedExcludes(t *testing.T) {
	config := isolateGit(t)
	if err := os.MkdirAll(filepath.Join(config, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "git", "ignore"), []byte("*.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := makeTree(t, "outside/a.tmp", "outside/.gitignore=*.bak\n", "outside/b.bak",
		"repo/.jj/", "repo/a.tmp", "repo/c.txt")
	// .gitignore applies everywhere; global excludes only inside a repo.
	wantPaths(t, paths(t, root, Options{Gitignore: true}),
		"outside", "outside/a.tmp", "repo", "repo/c.txt")
}

func TestAncestorRulesCoveringRootAreDropped(t *testing.T) {
	isolateGit(t)
	repo := makeTree(t, ".git/", ".gitignore=target/\n*.o\n", "target/sub/a.o", "target/sub/b.txt")
	wantPaths(t, paths(t, filepath.Join(repo, "target", "sub"), Options{Gitignore: true}), "b.txt")
	wantPaths(t, paths(t, filepath.Join(repo, "target"), Options{Gitignore: true}), "sub", "sub/b.txt")
	wantPaths(t, paths(t, repo, Options{Gitignore: true}))
}

func TestSortedCollectMatchesPathOrder(t *testing.T) {
	isolateGit(t)
	root := makeTree(t, "a/x", "a-b", "a.c", "B", "_", "a/y/z", "a0", "é")
	entries, err := Collect(t.Context(), root, Options{Sorted: true, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"B", "_", "a", "a-b", "a.c", "a/x", "a/y", "a/y/z", "a0", "é"}
	if got := entryPaths(entries); !slices.Equal(got, wantOrder) {
		t.Errorf("got %q, want %q", got, wantOrder)
	}
}

func TestVisitorActions(t *testing.T) {
	isolateGit(t)
	root := makeTree(t, "keep/a", "skip/b", "skip/c/d")
	var seen []string
	err := Walk(t.Context(), root, Options{Workers: 1}, func(_ *Worker, e *Entry) Action {
		seen = append(seen, strings.Clone(e.Path))
		if e.Path == "skip" {
			return SkipDir
		}
		return Continue
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(seen)
	wantPaths(t, seen, "keep", "keep/a", "skip")

	visits := 0
	err = Walk(t.Context(), root, Options{Workers: 1}, func(*Worker, *Entry) Action {
		visits++
		return Stop
	})
	if err != nil || visits != 1 {
		t.Errorf("Stop: visits=%d err=%v, want 1 visit", visits, err)
	}
}

func TestFullDetail(t *testing.T) {
	isolateGit(t)
	root := makeTree(t, "dir/five=12345", "empty=")
	entries, err := Collect(t.Context(), root, Options{Detail: fsys.Full, Sorted: true})
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{"dir": -1, "dir/five": 5, "empty": 0}
	for _, e := range entries {
		if e.Size != sizes[e.Path] {
			t.Errorf("%s: size %d, want %d", e.Path, e.Size, sizes[e.Path])
		}
		st, err := os.Lstat(filepath.Join(root, e.Path))
		if err != nil {
			t.Fatal(err)
		}
		if e.Mtime != st.ModTime().UnixNano() {
			t.Errorf("%s: mtime %d, want %d", e.Path, e.Mtime, st.ModTime().UnixNano())
		}
	}
}

func TestSymlinksAreReportedNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges")
	}
	isolateGit(t)
	root := makeTree(t, "real/f")
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	entries, err := Collect(t.Context(), root, Options{Sorted: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Path+":"+e.Type.String())
	}
	slices.Sort(got)
	wantPaths(t, got, "link:symlink", "real:dir", "real/f:file")
}

func TestFollowLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges")
	}
	isolateGit(t)
	root := makeTree(t, "real/f=12345", "a/b/g", ".gitignore=ignored\n", "ignored/x")
	links := map[string]string{
		"link":       "real",    // directory: descended
		"file":       "real/f",  // file: reported as its target
		"dangling":   "missing", // dropped
		"a/b/up":     "..",      // encloses the link: dropped
		"a/b/top":    "../..",   // the root: dropped
		"a/sibling":  "../real", // not enclosing: descended again
		"ignoredlnk": "ignored", // descended; ignore rules apply by walk path
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := Collect(t.Context(), root, Options{Sorted: true, Gitignore: true, FollowLinks: true, Detail: fsys.Full})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Path+":"+e.Type.String())
		if e.Path == "file" && e.Size != 5 {
			t.Errorf("followed file link size %d, want the target's 5", e.Size)
		}
	}
	slices.Sort(got)
	wantPaths(t, got,
		"a:dir", "a/b:dir", "a/b/g:file", "a/sibling:dir", "a/sibling/f:file",
		"file:file", "ignoredlnk:dir", "ignoredlnk/x:file", "link:dir", "link/f:file", "real:dir", "real/f:file")

	// Cached listings re-resolve links: retargeting one shows the new tree.
	c := NewCache(1 << 20)
	opts := Options{Sorted: true, FollowLinks: true, Cache: c}
	if _, err := Collect(t.Context(), root, opts); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	settle(t)
	got = got[:0]
	cached, err := Collect(t.Context(), root, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cached {
		if strings.HasPrefix(e.Path, "link/") {
			got = append(got, e.Path)
		}
	}
	slices.Sort(got)
	wantPaths(t, got, "link/b", "link/b/g", "link/sibling", "link/sibling/f")
}

func TestOpenByPathFallback(t *testing.T) {
	isolateGit(t)
	root := makeTree(t, ".gitignore=*.o\n", "a/b/c/d.o", "a/b/c/e", "a/f", "g/h/i")
	want := paths(t, root, Options{Gitignore: true, Workers: 2})
	saved := maxRetainedDirs
	maxRetainedDirs = 0
	defer func() { maxRetainedDirs = saved }()
	if got := paths(t, root, Options{Gitignore: true, Workers: 2}); !slices.Equal(got, want) {
		t.Errorf("open-by-path walk %q, want %q", got, want)
	}
}

// Walks over any io/fs.FS see the same filtering as native ones.
func TestWalkIOFS(t *testing.T) {
	isolateGit(t)
	mem := fstest.MapFS{
		".gitignore":     {Data: []byte("*.log\n")},
		"a.txt":          {},
		"x.log":          {},
		"sub/b.txt":      {},
		"sub/.ignore":    {Data: []byte("b.txt\n")},
		"sub/deep/c.txt": {},
		"node_modules/m": {},
		".hidden/d.txt":  {},
	}
	got := paths(t, "/", Options{FS: fsys.FromIOFS(mem), Gitignore: true, SkipNodeModules: true})
	wantPaths(t, got, "a.txt", "sub", "sub/deep", "sub/deep/c.txt")
}

func TestRootErrors(t *testing.T) {
	root := makeTree(t, "file")
	for _, bad := range []string{filepath.Join(root, "missing"), filepath.Join(root, "file")} {
		if err := Walk(t.Context(), bad, Options{}, func(*Worker, *Entry) Action { return Continue }); err == nil {
			t.Errorf("Walk(%s): want error", bad)
		}
	}
}
