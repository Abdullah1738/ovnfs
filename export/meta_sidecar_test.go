package export

import (
	"os"
	"testing"

	"github.com/stencil-hq/ovnfs/overlay"
)

// newMetadataTestFS uses the production overlay's filesystem and metadata methods.
func newMetadataTestFS(t *testing.T) *nodeFS {
	t.Helper()
	fs, err := overlay.New(t.TempDir(), nil, nil, overlay.NewMemory(64<<20, 1<<20, 0))
	if err != nil {
		t.Fatal(err)
	}
	return &nodeFS{Filesystem: fs, Change: fs, nodes: newNodeTable()}
}

// TestHiddenSidecarsRemainAddressable verifies hiding does not break xattr writes.
func TestHiddenSidecarsRemainAddressable(t *testing.T) {
	n := newMetadataTestFS(t)
	if err := n.MkdirAll("co/empty", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := n.Create("co/empty/._file")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	id := n.nodes.id("co/empty/._file")
	if _, err := n.Lstat("co/empty/._file"); err != nil {
		t.Fatal(err)
	}
	infos, err := n.ReadDir("co/empty")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("sidecar listed: %v", infos)
	}
	if err := n.Remove("co/empty"); err != nil {
		t.Fatal(err)
	}
	if _, ok := n.nodes.path(id); ok {
		t.Fatal("sidecar handle not retired")
	}
}

// TestSidecarsDoNotPermitRemovingVisibleChildren preserves real directory contents.
func TestSidecarsDoNotPermitRemovingVisibleChildren(t *testing.T) {
	n := newMetadataTestFS(t)
	if err := n.MkdirAll("co/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"co/dir/file", "co/dir/._file"} {
		f, err := n.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if err := n.Remove("co/dir"); err == nil {
		t.Fatal("removed nonempty directory")
	}
	if _, err := n.Stat("co/dir/file"); os.IsNotExist(err) {
		t.Fatal("visible child removed")
	}
}

// TestCheckoutRetirementLeavesOtherIdentitiesLive checks the checkout boundary.
func TestCheckoutRetirementLeavesOtherIdentitiesLive(t *testing.T) {
	nt := newNodeTable()
	a, b := nt.id("a/file"), nt.id("ab/file")
	e := &Export{fs: &nodeFS{nodes: nt}}
	e.Retire("a")
	if _, ok := nt.path(a); ok {
		t.Fatal("removed checkout handle lives")
	}
	if got := pathOf(t, nt, b); got != "ab/file" {
		t.Fatalf("other checkout: %s", got)
	}
}
