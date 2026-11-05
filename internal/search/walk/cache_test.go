package walk

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// settle lets every stamp in the tree age past the racy window so that the
// next cached walk stores listings.
func settle(t *testing.T) {
	t.Helper()
	saved := racyWindow
	racyWindow = 20 * time.Millisecond
	t.Cleanup(func() { racyWindow = saved })
	time.Sleep(2 * racyWindow)
}

// cachedMatchesFresh walks root with and without c and requires identical
// results, returning them.
func cachedMatchesFresh(t *testing.T, root string, c *Cache, opts Options) []string {
	t.Helper()
	fresh := paths(t, root, opts)
	opts.Cache = c
	if cached := paths(t, root, opts); !slices.Equal(cached, fresh) {
		t.Fatalf("cached walk %q\nfresh walk  %q", cached, fresh)
	}
	return fresh
}

func TestCacheFollowsTreeChanges(t *testing.T) {
	isolateGit(t)
	root := makeTree(t,
		".git/info/",
		".gitignore=*.log\n",
		"a/b/c/keep.txt", "a/b/c/drop.log", "a/x.txt", "gone.txt", "swap", "moved/inner.txt")
	c := NewCache(1 << 20)
	opts := Options{Gitignore: true, Workers: 2}
	steps := []struct {
		name   string
		mutate func() error
	}{
		{"add deep file", func() error { return os.WriteFile(filepath.Join(root, "a/b/c/new.txt"), nil, 0o644) }},
		{"remove file", func() error { return os.Remove(filepath.Join(root, "gone.txt")) }},
		{"rename dir", func() error { return os.Rename(filepath.Join(root, "moved"), filepath.Join(root, "renamed")) }},
		{"replace file with dir", func() error {
			if err := os.Remove(filepath.Join(root, "swap")); err != nil {
				return err
			}
			return os.MkdirAll(filepath.Join(root, "swap/child"), 0o755)
		}},
		// Editing an ignore file leaves its directory's stamp alone.
		{"edit .gitignore", func() error { return os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.txt\n"), 0o644) }},
		// So does creating the exclude file inside .git/info.
		{"create exclude", func() error {
			return os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte("a/\n"), 0o644)
		}},
	}
	for _, step := range steps {
		// Age the tree, then fill the cache, so the mutation below is
		// checked against trusted cached listings.
		settle(t)
		cachedMatchesFresh(t, root, c, opts)
		if err := step.mutate(); err != nil {
			t.Fatal(err)
		}
		got := cachedMatchesFresh(t, root, c, opts)
		if step.name == "add deep file" && !slices.Contains(got, "a/b/c/new.txt") {
			t.Errorf("%s: new file missing from %q", step.name, got)
		}
	}
}

// A change landing in the same timestamp tick as a listing leaves the
// directory's stamp unchanged, so a just-modified directory must not be
// served from the cache on the next walk.
func TestCacheNeverServesFreshListings(t *testing.T) {
	isolateGit(t)
	root := makeTree(t, "d/a")
	c := NewCache(1 << 20)
	for i := range 20 {
		if _, err := Collect(t.Context(), root, Options{Cache: c}); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(root, "d", "f"+string(rune('a'+i)))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		got := paths(t, root, Options{Cache: c})
		if !slices.Contains(got, "d/f"+string(rune('a'+i))) {
			t.Fatalf("iteration %d: walk right after a change missed it: %q", i, got)
		}
	}
}

func TestCacheEvictionKeepsResultsCorrect(t *testing.T) {
	isolateGit(t)
	root := makeTree(t, "a/1", "b/2", "c/3", "d/e/4")
	settle(t)
	c := NewCache(1) // every store overflows and evicts
	for range 3 {
		cachedMatchesFresh(t, root, c, Options{})
	}
}
