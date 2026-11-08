package index

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

func TestRacySmudgeMatchesGit(t *testing.T) {
	forEachFormat(t, func(t *testing.T, f object.Format) {
		r := newTestRepo(t, f)
		stamp := time.Unix(1767222000, 500_000_000)
		files := []string{"unchanged", "same-size", "resized", "deleted"}
		for _, name := range files {
			r.writeFile(name, "hello\n")
			if err := os.Chtimes(r.path(name), stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
		r.git(append([]string{"update-index", "--add"}, files...)...)
		// Modify in place, restoring the mtime: only the content (and ctime) tell.
		r.writeFile("same-size", "HELLO\n")
		r.writeFile("resized", "HELLO, WORLD\n")
		for _, name := range []string{"same-size", "resized"} {
			if err := os.Chtimes(r.path(name), stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(r.path("deleted")); err != nil {
			t.Fatal(err)
		}
		other := r.blob("other")

		for name, indexTime := range map[string]time.Time{
			"same instant":             stamp,
			"same second, index older": stamp.Add(-300 * time.Millisecond),
			"same second, index newer": stamp.Add(300 * time.Millisecond),
			"index a second older":     stamp.Add(-time.Second),
			"index a second newer":     stamp.Add(time.Second),
		} {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				base := filepath.Join(dir, "base")
				copyFile(t, r.path(".git/index"), base)
				if err := os.Chtimes(base, indexTime, indexTime); err != nil {
					t.Fatal(err)
				}
				gitIndex, ourIndex, blind := filepath.Join(dir, "git"), filepath.Join(dir, "ours"), filepath.Join(dir, "blind")
				for _, name := range []string{gitIndex, ourIndex, blind} {
					copyFile(t, base, name)
				}
				r.run([]string{"GIT_INDEX_FILE=" + gitIndex}, "100644 "+other.String()+"\tother\x00", "update-index", "-z", "--index-info")
				for _, target := range []struct {
					name     string
					worktree string
				}{{ourIndex, r.dir}, {blind, ""}} {
					idx := mustRead(t, target.name, f, ReadOptions{})
					if err := idx.Add(Entry{Path: "other", Mode: object.ModeFile, ID: other}); err != nil {
						t.Fatal(err)
					}
					if err := Write(target.name, idx, WriteOptions{Worktree: target.worktree}); err != nil {
						t.Fatal(err)
					}
				}
				if got, want := r.gitDebug(ourIndex), r.gitDebug(gitIndex); got != want {
					t.Fatalf("stat data:\n got %s\nwant %s", got, want)
				}
				if !bytes.Equal(readFileBytes(t, ourIndex), readFileBytes(t, gitIndex)) {
					t.Fatal("index bytes differ")
				}

				racy := indexTime.Unix() <= stamp.Unix()
				for _, e := range mustRead(t, ourIndex, f, ReadOptions{}).Entries {
					if e.Path != "same-size" && e.Path != "other" && e.Stat.Size == 0 {
						t.Errorf("%s smudged", e.Path)
					}
					if e.Path == "same-size" && !racy && e.Stat.Size == 0 {
						t.Errorf("%s smudged though not racy", e.Path)
					}
					// The rewrite changed the ctime; it still matches the
					// cached one unless a second boundary passed in between.
					if st, ok := platformStat(r.path(e.Path)); e.Path == "same-size" && racy && ok &&
						st.CTime.Sec == e.Stat.CTime.Sec && e.Stat.Size != 0 {
						t.Errorf("%s not smudged", e.Path)
					}
				}
				// Without a work tree every racily clean entry is smudged.
				for _, e := range mustRead(t, blind, f, ReadOptions{}).Entries {
					if e.Path != "other" && (e.Stat.Size == 0) != racy {
						t.Errorf("blind: %s size %d, racy %v", e.Path, e.Stat.Size, racy)
					}
				}
			})
		}
	})
}
