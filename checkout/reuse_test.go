package checkout

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// reuseRepo is a committed repository with an ignored build directory.
func reuseRepo(t *testing.T) string {
	t.Helper()
	dir := folder(t, map[string]string{"a.txt": "one", ".gitignore": "build/\n", "build/out": "o"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "init")
	return dir
}

// checkoutHead returns the commit HEAD names in a checkout's private git directory.
func (f *fixture) checkoutHead(name string) string {
	f.t.Helper()
	return gitRun(f.t, f.t.TempDir(), "--git-dir", f.gitdir(name), "rev-parse", "HEAD")
}

// prepared counts the checkouts the folder at dir holds prepared.
func (f *fixture) prepared(dir string) int {
	f.reg.mu.RLock()
	defer f.reg.mu.RUnlock()
	if folder := f.reg.folders[dir]; folder != nil {
		return len(folder.pool)
	}
	return 0
}

// poolFull reports whether the folder at dir holds as many prepared checkouts as its pool should.
func (f *fixture) poolFull(dir string) bool {
	f.reg.mu.RLock()
	defer f.reg.mu.RUnlock()
	folder := f.reg.folders[dir]
	return folder != nil && len(folder.pool) >= folder.poolSize
}

// TestFolderCreatesReuseUnchangedCapture opens later checkouts of an unchanged
// folder on the earlier capture, and captures again as soon as the folder
// changes in any way a checkout shows: source content, or the repository's
// commits. Edits below a cache path change nothing a capture holds. Checkouts
// prepared before a source edit serve the new capture, with working git.
func TestFolderCreatesReuseUnchangedCapture(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := reuseRepo(t)
	first := f.newCheckout(dir, "first").Checkout
	second := f.newCheckout(dir, "second").Checkout
	if second.Base != first.Base {
		t.Fatalf("unchanged folder captured again: base %s, then %s", first.Base, second.Base)
	}
	if got := f.read("second", "a.txt"); got != "one" {
		t.Fatalf("reused capture serves %q", got)
	}

	write(t, filepath.Join(dir, "build/out"), "rebuilt")
	if got := f.newCheckout(dir, "cache").Checkout.Base; got != first.Base {
		t.Fatalf("an edit below a cache path forced a capture: base %s, want %s", got, first.Base)
	}
	eventually(t, "the pool did not fill", func() bool { return f.poolFull(dir) })
	write(t, filepath.Join(dir, "a.txt"), "two")
	edited := f.newCheckout(dir, "edited").Checkout
	if edited.Base == first.Base {
		t.Fatal("a source edit made just before the create reused the old capture")
	}
	if got := f.read("edited", "a.txt"); got != "two" {
		t.Fatalf("checkout after a source edit serves %q", got)
	}
	if got := f.read("second", "a.txt"); got != "one" {
		t.Fatalf("earlier checkout changed to %q", got)
	}
	// The recapture wrote its git template in the background; the pool fills again from it.
	eventually(t, "the pool did not refill after the recapture", func() bool { return f.poolFull(dir) })
	// A checkout prepared for the recapture, from its template, serves it.
	if got := f.newCheckout(dir, "later").Checkout.Base; got != edited.Base {
		t.Fatalf("create after the recapture on base %s, want %s", got, edited.Base)
	}
	if got := f.read("later", "a.txt"); got != "two" {
		t.Fatalf("prepared checkout serves %q after the edit", got)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("later"), "cat-file", "-t", "HEAD^{tree}"); got != "tree" {
		t.Fatalf("prepared checkout cannot read its objects: %q", got)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("later"), "config", "ovnfs.checkout"); got != "later" {
		t.Fatalf("checkout marker = %q", got)
	}

	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "later")
	committed := f.newCheckout(dir, "committed").Checkout
	if committed.Base == edited.Base {
		t.Fatal("a commit made just before the create reused the old capture")
	}
	if got, want := f.checkoutHead("committed"), gitRun(t, dir, "rev-parse", "HEAD"); got != want {
		t.Fatalf("checkout HEAD %s, want the folder's %s", got, want)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("committed"), "config", "ovnfs.checkout"); got != "committed" {
		t.Fatalf("checkout marker = %q", got)
	}

	if got := f.newCheckout(dir, "rules", "local a.txt").Checkout.Base; got == committed.Base {
		t.Fatal("a create with other rules reused a capture classified by different ones")
	}
}

// TestPreparedCheckoutsOutsideRepositories serves checkouts of a folder outside
// any repository from prepared ones as well, with no git directory.
func TestPreparedCheckoutsOutsideRepositories(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := folder(t, map[string]string{"a.txt": "one"})
	first := f.newCheckout(dir, "first").Checkout
	eventually(t, "no checkout was prepared", func() bool { return f.prepared(dir) > 0 })
	second := f.newCheckout(dir, "second").Checkout
	if second.Base != first.Base || second.Git {
		t.Fatalf("second = %+v, want a checkout without git on base %s", second, first.Base)
	}
	f.put("second", "a.txt", "two")
	if got, other := f.read("second", "a.txt"), f.read("first", "a.txt"); got != "two" || other != "one" {
		t.Fatalf("second holds %q, first %q", got, other)
	}
}

// TestUnnamedFolderCreates opens concurrent checkouts with registry-chosen names,
// each its own, and leaves no files behind for prepared checkouts nobody took
// once the registry closes.
func TestUnnamedFolderCreates(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := reuseRepo(t)
	const n = 12
	replies := make([]api.NewReply, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			replies[i], errs[i] = f.reg.New(context.Background(), api.NewArgs{Dir: dir})
		})
	}
	wg.Wait()
	names := map[string]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		name := replies[i].Checkout.Name
		if names[name] {
			t.Fatalf("two creates got checkout %s", name)
		}
		names[name] = true
		f.put(name, "a.txt", name)
	}
	for name := range names {
		if got := f.read(name, "a.txt"); got != name {
			t.Fatalf("%s holds %q: checkouts share files", name, got)
		}
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: replies[0].Checkout.Name}); err != nil {
		t.Fatal(err)
	}
	delete(names, replies[0].Checkout.Name)

	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	var live []string
	for _, c := range f.reg.Ls().Checkouts {
		live = append(live, c.Name)
	}
	var want []string
	for name := range names {
		want = append(want, name)
	}
	slices.Sort(want)
	if !slices.Equal(live, want) {
		t.Fatalf("after reopen checkouts %v, want %v", live, want)
	}
	// No create ran since the reopen, so nothing is prepared: every directory left
	// belongs to a live checkout.
	for _, kind := range []string{"uppers", "git"} {
		entries, err := os.ReadDir(filepath.Join(f.state, kind))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(want) {
			t.Fatalf("%s holds %d directories for %d live checkouts", kind, len(entries), len(want))
		}
	}
	for name := range names {
		if got := f.read(name, "a.txt"); got != name {
			t.Fatalf("%s after reopen holds %q", name, got)
		}
	}
}
