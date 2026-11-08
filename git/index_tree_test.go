package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/clone"
)

func TestIndexTreeUsesStageZeroObjectsAndCapturedIndex(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	// Added by object id: Windows cannot create this file name, but its index can
	// hold it once Git for Windows' NTFS path check is off.
	name := "tab\tand\nnewline"
	blob := filepath.Join(t.TempDir(), "blob")
	testWrite(t, blob, "unusual filename\n")
	testGit(t, r.Top, "-c", "core.protectNTFS=false", "update-index", "--add", "--cacheinfo", "100644,"+testGit(t, r.Top, "hash-object", "-w", "--", blob)+","+name)
	testGit(t, r.Top, "update-index", "--skip-worktree", "tracked")
	oid := testGit(t, r.Top, "rev-parse", ":tracked")
	testGit(t, r.Top, "update-index", "--add", "--cacheinfo", "100755,"+oid+",statless")
	cmd := r.command(ctx, "update-index", "--index-info")
	cmd.Stdin = strings.NewReader("100644 " + oid + " 1\tconflicted\n100644 " + oid + " 2\tconflicted\n100644 " + oid + " 3\tconflicted\n")
	if out, err := run(cmd); err != nil {
		t.Fatalf("unmerged index: %s, %v", out, err)
	}
	index := filepath.Join(t.TempDir(), "captured-index")
	if err := clone.File(r.IndexPath(), index); err != nil {
		t.Fatal(err)
	}
	testGit(t, r.Top, "update-index", "--force-remove", "statless")
	entries, err := r.IndexTree(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]TreeEntry{}
	for _, e := range entries {
		got[e.Path] = e
	}
	if len(got) != 3 {
		t.Fatalf("stage-zero entries = %+v", entries)
	}
	for p, size := range map[string]int64{"tracked": int64(len("original\n")), "statless": int64(len("original\n")), name: int64(len("unusual filename\n"))} {
		if got[p].Size != size || got[p].Type != "blob" {
			t.Errorf("%q metadata = %+v, want blob of size %d", p, got[p], size)
		}
	}
	if got["statless"].Mode != 0o100755 {
		t.Fatalf("statless executable mode = %o", got["statless"].Mode)
	}
}

func TestIndexTreeMissingObjectFails(t *testing.T) {
	r := testRepo(t)
	testGit(t, r.Top, "update-index", "--add", "--cacheinfo", "100644,"+strings.Repeat("1", 40)+",missing")
	if _, err := r.IndexTree(context.Background(), r.IndexPath()); err == nil {
		t.Fatal("missing index object was accepted")
	}
}
