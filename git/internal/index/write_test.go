package index

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

func TestWriteLock(t *testing.T) {
	name := filepath.Join(t.TempDir(), "index")
	idx := New(object.SHA1)
	if err := os.WriteFile(name+".lock", nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := Write(name, idx, WriteOptions{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("Write with held lock: %v", err)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("index written despite the lock")
	}
	if err := os.Remove(name + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := Write(name, idx, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lock left behind")
	}
	info, _ := os.Stat(name)
	if !idx.ModTime.Equal(info.ModTime()) {
		t.Fatal("ModTime not updated to the written file")
	}
}

// TestRoundTrip rewrites git-written indexes and checks that what git can
// keep is kept byte for byte, and what it cannot is dropped cleanly.
func TestRoundTrip(t *testing.T) {
	forEachFormat(t, func(t *testing.T, f object.Format) {
		identical := map[string]func(r *testRepo){
			"v2 with resolve-undo": func(r *testRepo) {
				input, _ := r.indexInfo([]edit{
					{path: "top"},
					{mode: object.ModeFile, path: "top", content: "1", stage: 1},
					{mode: object.ModeFile, path: "top", content: "2", stage: 2},
				})
				r.run(nil, input, "update-index", "-z", "--index-info")
				r.git("add", "top")
			},
			"v3": func(r *testRepo) {
				r.writeFile("new", "new\n")
				r.git("add", "-N", "new")
				r.git("update-index", "--skip-worktree", "b/1")
			},
			"v4": func(r *testRepo) { r.git("update-index", "--index-version", "4") },
		}
		for name, setup := range identical {
			t.Run(name, func(t *testing.T) {
				r := populate(t, f)
				setup(r)
				src := r.path(".git/index")
				out := filepath.Join(t.TempDir(), "index")
				if err := WriteFile(out, mustRead(t, src, f, ReadOptions{}), WriteOptions{Worktree: r.dir}); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(readFileBytes(t, out), readFileBytes(t, src)) {
					t.Fatalf("rewritten index differs:\n got %x\nwant %x", readFileBytes(t, out), readFileBytes(t, src))
				}
			})
		}

		t.Run("dropped extensions", func(t *testing.T) {
			r := populate(t, f)
			r.git("update-index", "--split-index")
			r.writeFile("a/x", "changed\n")
			r.git("update-index", "a/x")
			r.writeFile("u", "")
			r.git("-c", "core.untrackedCache=true", "status", "--porcelain")
			src := r.path(".git/index")
			idx := mustRead(t, src, f, ReadOptions{})
			if idx.Untracked == nil {
				t.Fatal("no untracked cache to drop")
			}
			out := filepath.Join(t.TempDir(), "index")
			if err := WriteFile(out, idx, WriteOptions{Worktree: r.dir}); err != nil {
				t.Fatal(err)
			}
			data := readFileBytes(t, out)
			for _, sig := range []string{"link", "UNTR"} {
				if bytes.Contains(data, []byte(sig)) {
					t.Errorf("%s written", sig)
				}
			}
			if got, want := r.withIndex(out, "ls-files", "--stage", "-z"), r.withIndex(src, "ls-files", "--stage", "-z"); got != want {
				t.Fatalf("entries:\n got %q\nwant %q", got, want)
			}
			if got, want := r.gitDebug(out), r.gitDebug(src); got != want {
				t.Fatalf("stat data:\n got %s\nwant %s", got, want)
			}
		})

		t.Run("extended flags pick the version", func(t *testing.T) {
			r := populate(t, f)
			out := filepath.Join(t.TempDir(), "index")
			idx := mustRead(t, r.path(".git/index"), f, ReadOptions{})
			i, _ := idx.Find("b/1", 0)
			idx.Entries[i].SkipWorktree = true
			if err := WriteFile(out, idx, WriteOptions{Worktree: r.dir}); err != nil {
				t.Fatal(err)
			}
			if v := indexVersion(t, out); v != 3 {
				t.Fatalf("version %d with skip-worktree", v)
			}
			if got := r.withIndex(out, "ls-files", "-t", "b/1"); got != "S b/1\n" {
				t.Fatalf("git ls-files -t: %q", got)
			}
			idx.Entries[i].SkipWorktree = false
			if err := WriteFile(out, idx, WriteOptions{Worktree: r.dir}); err != nil {
				t.Fatal(err)
			}
			if v := indexVersion(t, out); v != 2 {
				t.Fatalf("version %d without extended flags", v)
			}
		})
	})
}
