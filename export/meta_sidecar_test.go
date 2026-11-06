package export

import (
	"os"
	"testing"

	"github.com/stencil-hq/ovnfs/ntinfo"
	"github.com/stencil-hq/ovnfs/overlay"
)

// newMetadataTestFS uses the production overlay's filesystem and metadata methods.
func newMetadataTestFS(t *testing.T) *nodeFS {
	t.Helper()
	fs, err := overlay.New(t.TempDir(), nil, nil, overlay.NewMemory(64<<20, 1<<20, 0))
	if err != nil {
		t.Fatal(err)
	}
	return &nodeFS{Filesystem: fs, Change: fs, nodes: newNodeTable(fs.ReadDir)}
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

// TestDirectoryLinksAreDirectories describes a symlink to a directory as a
// directory symlink (FILE_ATTRIBUTE_DIRECTORY|REPARSE_POINT) in lookups and
// listings, and other symlinks as file symlinks, as NTFS does.
func TestDirectoryLinksAreDirectories(t *testing.T) {
	n := newMetadataTestFS(t)
	if err := n.MkdirAll("co/packages/utils", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := n.Create("co/file")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	for link, target := range map[string]string{"co/dirlink": "packages/utils", "co/filelink": "file", "co/dangling": "missing"} {
		if err := n.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]uint32{"dirlink": 0x410, "filelink": 0x420, "dangling": 0x420}
	for name, attrs := range want {
		info, err := n.Lstat("co/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if got := ntinfo.Attributes(info); got != attrs {
			t.Errorf("lstat %s attributes = %#x, want %#x", name, got, attrs)
		}
	}
	infos, err := n.ReadDir("co")
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if attrs, ok := want[info.Name()]; ok && ntinfo.Attributes(info) != attrs {
			t.Errorf("listed %s attributes = %#x, want %#x", info.Name(), ntinfo.Attributes(info), attrs)
		}
	}
}
