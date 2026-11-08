package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stencil-hq/ovnfs/store"
)

// TestReuseKeyFollowsWalkedMetadata keys equal captures alike, separates
// permission changes git trees cannot see, and refuses Results that are not
// fresh stored captures.
func TestReuseKeyFollowsWalkedMetadata(t *testing.T) {
	st, err := store.Open(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sub", "a.txt"), "a")
	first := capture(t, dir, st, "")
	key := first.ReuseKey()
	if key == "" || capture(t, dir, st, "").ReuseKey() != key {
		t.Fatalf("unchanged folder keys %q differently", key)
	}
	// Read-only modes, the only permission Windows retains; restored so cleanup can remove them.
	t.Cleanup(func() {
		os.Chmod(filepath.Join(dir, "sub"), 0o755)
		os.Chmod(filepath.Join(dir, "sub", "a.txt"), 0o644)
	})
	if err := os.Chmod(filepath.Join(dir, "sub", "a.txt"), 0o444); err != nil {
		t.Fatal(err)
	}
	chmodded := capture(t, dir, st, "").ReuseKey()
	if chmodded == key || chmodded == "" {
		t.Fatalf("file mode change kept key %q", chmodded)
	}
	if err := os.Chmod(filepath.Join(dir, "sub"), 0o555); err != nil {
		t.Fatal(err)
	}
	if got := capture(t, dir, st, "").ReuseKey(); got == "" || got == chmodded {
		t.Fatalf("directory mode change kept key %q", got)
	}

	data, err := first.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var decoded Result
	if err := decoded.UnmarshalBinary(data); err != nil {
		t.Fatal(err)
	}
	if got := decoded.ReuseKey(); got != "" {
		t.Fatalf("decoded capture keyed %q", got)
	}
	if got := capture(t, dir, nil, "").ReuseKey(); got != "" {
		t.Fatalf("hash-only capture keyed %q", got)
	}
}
