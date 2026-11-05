package checkout

import (
	"path/filepath"
	"testing"
)

// TestFolderEditsInvalidateLivePaths recalls clients' caches of cache paths
// edited in the source folder, while captured source stays pinned and is
// neither invalidated nor ever untracked.
func TestFolderEditsInvalidateLivePaths(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := folder(t, map[string]string{"src/main": "one", "target/out": "one"})
	f.newCheckout(dir, "app", "cache dir & target")
	if !f.reg.Tracked("app", "src/main") {
		t.Fatal("captured source must be tracked: the folder cannot change it")
	}
	eventually(t, "the watcher never covered the folder", func() bool { return f.reg.Tracked("app", "target/out") })
	// Source first: had it been invalidated, that event would not arrive after target's.
	write(t, filepath.Join(dir, "src/main"), "two")
	write(t, filepath.Join(dir, "target/out"), "two")
	eventually(t, "a folder edit to a cache path was not invalidated", func() bool { return f.invalidatedPath("app", "target/out") })
	if f.invalidatedPath("app", "src/main") {
		t.Fatal("a folder edit to captured source was invalidated")
	}
	if got := f.read("app", "target/out"); got != "two" {
		t.Fatalf("live cache path = %q", got)
	}
	if got := f.read("app", "src/main"); got != "one" {
		t.Fatalf("captured source = %q", got)
	}
}
