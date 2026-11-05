package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchBranchFileDirectoryReplacement(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "staged-collision"}[staged], func(t *testing.T) {
			r := testRepo(t)
			testWrite(t, filepath.Join(r.Top, "dir", "old"), "base\n")
			testGit(t, r.Top, "add", "--all")
			testGit(t, r.Top, "commit", "-qm", "directory")
			gitdir, work := branchCheckout(t, r)
			if err := os.RemoveAll(filepath.Join(work, "dir")); err != nil {
				t.Fatal(err)
			}
			testWrite(t, filepath.Join(work, "dir"), "replacement\n")
			testGit(t, work, "add", "--all")
			testGit(t, work, "commit", "-qm", "replace directory")
			if staged {
				testWrite(t, filepath.Join(r.Top, "dir", "unrelated"), "staged\n")
				testGit(t, r.Top, "add", "dir/unrelated")
			}
			before, old := indexBytes(t, r), testGit(t, r.Top, "rev-parse", "HEAD")
			for _, dry := range []bool{true, false} {
				_, _, err := r.FetchBranch(context.Background(), gitdir, "main", dry)
				if staged {
					if !errors.Is(err, ErrBranchConflict) {
						t.Fatalf("staged collision = %v", err)
					}
					if !bytes.Equal(before, indexBytes(t, r)) || testGit(t, r.Top, "rev-parse", "HEAD") != old {
						t.Fatal("conflict changed index or ref")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if !staged && testGit(t, r.Top, "ls-files", "dir") != "dir" {
				t.Fatal("replacement index entry missing")
			}
		})
	}
}

func TestFetchBranchEmptyCommitPreservesIndex(t *testing.T) {
	r := testRepo(t)
	gitdir, work := branchCheckout(t, r)
	testGit(t, work, "commit", "-qm", "empty", "--allow-empty")
	testWrite(t, filepath.Join(r.Top, "tracked"), "staged\n")
	testGit(t, r.Top, "add", "tracked")
	before := indexBytes(t, r)
	commit, changes, err := r.FetchBranch(context.Background(), gitdir, "main", false)
	if err != nil || len(changes) != 0 || commit != testGit(t, work, "rev-parse", "HEAD") {
		t.Fatalf("apply = %s %+v %v", commit, changes, err)
	}
	if !bytes.Equal(before, indexBytes(t, r)) {
		t.Fatal("empty commit rewrote index")
	}
}
