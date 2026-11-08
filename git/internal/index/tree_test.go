package index

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// buildTree stores a tree holding paths (mode by path) using only the object
// database, so paths need not be valid on the host file system, and returns
// a commit of it.
func (r *testRepo) buildTree(files map[string]object.Mode) (tree, commit object.ID) {
	r.t.Helper()
	var info strings.Builder
	for path, mode := range files {
		id := r.blob("content of " + path)
		if mode == object.ModeGitlink {
			id = r.id(r.run(nil, "", "commit-tree", "-m", "sub", r.emptyTree().String()))
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", mode, id, path)
	}
	build := filepath.Join(r.t.TempDir(), "build-index")
	r.run([]string{"GIT_INDEX_FILE=" + build}, info.String(), "update-index", "-z", "--add", "--index-info")
	tree = r.id(r.withIndex(build, "write-tree"))
	commit = r.id(r.git("commit-tree", "-m", "c", tree.String()))
	return tree, commit
}

func (r *testRepo) emptyTree() object.ID {
	return r.id(r.run(nil, "", "hash-object", "-w", "-t", "tree", "--stdin"))
}

func TestFromTreeMatchesReadTree(t *testing.T) {
	long := strings.Repeat("d", 200)
	deep := strings.Repeat(long+"/", 21) + "leaf" // longer than the 12-bit name length
	files := map[string]object.Mode{
		"a":              object.ModeFile,
		"a-b/c":          object.ModeFile,
		"a.b":            object.ModeExec,
		"a/b/c/d":        object.ModeFile,
		"a/b/e":          object.ModeSymlink,
		"a/bb/x":         object.ModeFile,
		"a/c":            object.ModeFile,
		"bb/x":           object.ModeFile,
		"sub":            object.ModeGitlink,
		"z/with space/ü": object.ModeFile,
		"zz/1/2/3/4/5/6": object.ModeFile,
		deep:             object.ModeFile,
	}

	forEachFormat(t, func(t *testing.T, f object.Format) {
		r := newTestRepo(t, f)
		tree, commit := r.buildTree(files)
		for name, root := range map[string]object.ID{"full": tree, "empty": r.emptyTree()} {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				want := filepath.Join(dir, "git-index")
				rev := root.String()
				if name == "full" {
					rev = commit.String()
				}
				r.withIndex(want, "read-tree", rev)

				idx, err := FromTree(r.readTree, root, f)
				if err != nil {
					t.Fatal(err)
				}
				got := filepath.Join(dir, "index")
				if err := Write(got, idx, WriteOptions{}); err != nil {
					t.Fatal(err)
				}
				if g, w := readFileBytes(t, got), readFileBytes(t, want); !bytes.Equal(g, w) {
					t.Fatalf("index differs from git read-tree:\n got %x\nwant %x", g, w)
				}
				back := mustRead(t, got, f, ReadOptions{})
				if g, w := lsStage(back.Entries), r.withIndex(want, "ls-files", "--stage", "-z"); g != w {
					t.Fatalf("entries:\n got %q\nwant %q", g, w)
				}
			})
		}
	})
}
