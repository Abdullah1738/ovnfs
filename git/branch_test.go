package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

func branchCheckout(t *testing.T, r *Repo) (string, string) {
	t.Helper()
	state := t.TempDir()
	objects, gitdir, work := filepath.Join(state, "objects"), filepath.Join(state, "git"), filepath.Join(state, "work")
	if err := r.PrepareObjects(objects); err != nil {
		t.Fatal(err)
	}
	if err := r.MakeGitdir(gitdir, objects, r.IndexPath()); err != nil {
		t.Fatal(err)
	}
	for path := range strings.SplitSeq(testGit(t, r.Top, "ls-files"), "\n") {
		data, err := os.ReadFile(filepath.Join(r.Top, path))
		if err != nil {
			t.Fatal(err)
		}
		testWrite(t, filepath.Join(work, path), string(data))
	}
	if err := WriteGitfile(work, gitdir); err != nil {
		t.Fatal(err)
	}
	return gitdir, work
}

func indexBytes(t *testing.T, r *Repo) []byte {
	t.Helper()
	data, err := os.ReadFile(r.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFetchCheckedOutBranch(t *testing.T) {
	eachFetch(t, func(t *testing.T, fetch func(func())) {
		r := testRepo(t)
		for _, path := range []string{"a.go", "b.go", "c.go"} {
			testWrite(t, filepath.Join(r.Top, path), "base\n")
		}
		testGit(t, r.Top, "add", "--all")
		testGit(t, r.Top, "commit", "-qm", "files")
		gitdir, work := branchCheckout(t, r)
		testWrite(t, filepath.Join(work, "a.go"), "dirty a\n")
		testGit(t, work, "add", "a.go")
		testGit(t, work, "commit", "-qm", "agent a")
		want := testGit(t, work, "rev-parse", "HEAD")
		for _, path := range []string{"a.go", "b.go", "c.go"} {
			testWrite(t, filepath.Join(r.Top, path), "dirty "+strings.TrimSuffix(path, ".go")+"\n")
		}
		testGit(t, r.Top, "add", "c.go")
		old := testGit(t, r.Top, "rev-parse", "HEAD")
		before := indexBytes(t, r)
		var commit string
		var changes []BranchChange
		var err error
		fetch(func() { commit, changes, err = r.FetchBranch(context.Background(), gitdir, "main", true) })
		if err != nil || commit != want || len(changes) != 1 || changes[0] != (BranchChange{Path: "a.go", Status: "M"}) {
			t.Fatalf("dry run = %s %+v %v", commit, changes, err)
		}
		if !bytes.Equal(before, indexBytes(t, r)) || testGit(t, r.Top, "rev-parse", "HEAD") != old {
			t.Fatal("dry run changed index or ref")
		}
		if _, err := os.Stat(filepath.Join(r.CommonDir, "objects", want[:2], want[2:])); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dry run fetched commit: %v", err)
		}
		fetch(func() { commit, changes, err = r.FetchBranch(context.Background(), gitdir, "main", false) })
		if err != nil || commit != want || len(changes) != 1 {
			t.Fatalf("apply = %s %+v %v", commit, changes, err)
		}
		if got := testGit(t, r.Top, "status", "--porcelain"); got != "M b.go\nM  c.go" {
			t.Fatalf("status = %q", got)
		}
		if got := testGit(t, r.Top, "rev-parse", "HEAD"); got != want {
			t.Fatalf("HEAD = %s, want %s", got, want)
		}
		for _, path := range []string{"a.go", "b.go", "c.go"} {
			data, err := os.ReadFile(filepath.Join(r.Top, path))
			if err != nil || string(data) != "dirty "+strings.TrimSuffix(path, ".go")+"\n" {
				t.Fatalf("working tree %s = %q, %v", path, data, err)
			}
		}
		testGit(t, r.Top, "fsck", "--strict")
	})
}

// TestFetchCheckedOutBranchReplacesHeldIndex checks that the index is replaced
// while another process, such as an editor's git integration, holds it open
// sharing delete access, which Windows' plain rename refuses.
func TestFetchCheckedOutBranchReplacesHeldIndex(t *testing.T) {
	r := testRepo(t)
	testWrite(t, filepath.Join(r.Top, "a.go"), "base\n")
	testGit(t, r.Top, "add", "--all")
	testGit(t, r.Top, "commit", "-qm", "files")
	gitdir, work := branchCheckout(t, r)
	testWrite(t, filepath.Join(work, "a.go"), "agent\n")
	testGit(t, work, "commit", "-qam", "agent a")
	want := testGit(t, work, "rev-parse", "HEAD")
	held, err := hostfile.Open(r.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	commit, changes, err := r.FetchBranch(context.Background(), gitdir, "main", false)
	if err != nil || commit != want || len(changes) != 1 {
		t.Fatalf("apply = %s %+v %v", commit, changes, err)
	}
	if got := testGit(t, r.Top, "status", "--porcelain"); got != "M a.go" {
		t.Fatalf("status = %q, want the working tree's old a.go against the new index", got)
	}
}

// TestFetchCheckedOutBranchKeepsRacyEdits covers a same-size edit made in the
// second its file was staged: only the index's own mtime tells git to recheck it.
func TestFetchCheckedOutBranchKeepsRacyEdits(t *testing.T) {
	r := testRepo(t)
	testGit(t, r.Top, "config", "core.trustctime", "false")
	past := time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, path := range []string{"a.go", "c.go"} {
		testWrite(t, filepath.Join(r.Top, path), "c1\n")
		if err := os.Chtimes(filepath.Join(r.Top, path), past, past); err != nil {
			t.Fatal(err)
		}
	}
	testGit(t, r.Top, "add", "--all")
	testGit(t, r.Top, "commit", "-qm", "files")
	gitdir, work := branchCheckout(t, r)
	testWrite(t, filepath.Join(work, "a.go"), "agent\n")
	testGit(t, work, "commit", "-qam", "agent a")
	testWrite(t, filepath.Join(r.Top, "c.go"), "c2\n")
	for _, name := range []string{filepath.Join(r.Top, "c.go"), r.IndexPath()} {
		if err := os.Chtimes(name, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := r.FetchBranch(context.Background(), gitdir, "main", false); err != nil {
		t.Fatal(err)
	}
	if got := testGit(t, r.Top, "status", "--porcelain", "--", "c.go"); got != "M c.go" {
		t.Fatalf("status of the racy edit = %q", got)
	}
}

func TestFetchCheckedOutBranchRefusals(t *testing.T) {
	for _, reason := range []string{"non-fast-forward", "staged", "index-lock", "ref-lock"} {
		t.Run(reason, func(t *testing.T) {
			r := testRepo(t)
			gitdir, work := branchCheckout(t, r)
			testWrite(t, filepath.Join(work, "tracked"), "agent\n")
			testGit(t, work, "commit", "-qam", "agent")
			switch reason {
			case "non-fast-forward":
				testGit(t, r.Top, "commit", "-qm", "diverged", "--allow-empty")
			case "staged":
				testWrite(t, filepath.Join(r.Top, "tracked"), "staged\n")
				testGit(t, r.Top, "add", "tracked")
			case "index-lock":
				testWrite(t, r.IndexPath()+".lock", "busy")
			case "ref-lock":
				testWrite(t, filepath.Join(r.CommonDir, "refs", "heads", "main.lock"), "busy")
			}
			old := testGit(t, r.Top, "rev-parse", "HEAD")
			before := indexBytes(t, r)
			_, _, err := r.FetchBranch(context.Background(), gitdir, "main", false)
			if err == nil {
				t.Fatal("unsafe apply succeeded")
			}
			if reason == "staged" || reason == "non-fast-forward" {
				if !errors.Is(err, ErrBranchConflict) || !strings.Contains(err.Error(), "different branch name") {
					t.Fatalf("conflict = %v", err)
				}
			}
			if reason == "staged" && !strings.Contains(err.Error(), "tracked") {
				t.Fatalf("conflicting path missing: %v", err)
			}
			if !bytes.Equal(before, indexBytes(t, r)) || testGit(t, r.Top, "rev-parse", "HEAD") != old {
				t.Fatal("refused apply changed index or ref")
			}
		})
	}
}

func TestFetchCheckedOutBranchDeletionAndMode(t *testing.T) {
	r := testRepo(t)
	gitdir, work := branchCheckout(t, r)
	if err := os.Remove(filepath.Join(work, "tracked")); err != nil {
		t.Fatal(err)
	}
	testWrite(t, filepath.Join(work, "executable"), "#!/bin/sh\n")
	testGit(t, work, "add", "--all")
	testGit(t, work, "update-index", "--chmod=+x", "executable")
	testGit(t, work, "commit", "-qm", "delete and add")
	_, changes, err := r.FetchBranch(context.Background(), gitdir, "main", false)
	if err != nil || len(changes) != 2 {
		t.Fatalf("apply = %+v %v", changes, err)
	}
	if got := testGit(t, r.Top, "ls-files", "--stage"); !strings.HasPrefix(got, "100755 ") || !strings.HasSuffix(got, "\texecutable") {
		t.Fatalf("index = %q", got)
	}
	if got := testGit(t, r.Top, "status", "--porcelain"); got != "D executable\n?? tracked" {
		t.Fatalf("working tree was changed: %q", got)
	}
}

func TestFetchOtherBranchKeepsIndex(t *testing.T) {
	eachFetch(t, func(t *testing.T, fetch func(func())) {
		r := testRepo(t)
		gitdir, work := branchCheckout(t, r)
		testWrite(t, filepath.Join(work, "tracked"), "agent\n")
		testGit(t, work, "commit", "-qam", "agent")
		old, before := testGit(t, r.Top, "rev-parse", "HEAD"), indexBytes(t, r)
		for _, branch := range []string{"agent", "agent"} {
			var commit string
			var changes []BranchChange
			var err error
			fetch(func() { commit, changes, err = r.FetchBranch(context.Background(), gitdir, branch, false) })
			if err != nil || len(changes) != 0 || commit != testGit(t, work, "rev-parse", "HEAD") {
				t.Fatalf("fetch = %s %+v %v", commit, changes, err)
			}
		}
		if !bytes.Equal(before, indexBytes(t, r)) || testGit(t, r.Top, "rev-parse", "HEAD") != old {
			t.Fatal("fetching another branch changed index or HEAD")
		}
		testGit(t, r.Top, "fsck", "--strict")
		var err error
		fetch(func() { _, _, err = r.FetchBranch(context.Background(), gitdir, "agent", true) })
		if !errors.Is(err, ErrBranchDryRun) {
			t.Fatalf("other branch dry run = %v", err)
		}
	})
}

// TestFetchOtherBranchRefusals refuses, with git and in process alike, to move
// a branch checked out in another work tree or one the checkout's HEAD does
// not fast-forward.
func TestFetchOtherBranchRefusals(t *testing.T) {
	eachFetch(t, func(t *testing.T, fetch func(func())) {
		r := testRepo(t)
		gitdir, work := branchCheckout(t, r)
		testWrite(t, filepath.Join(work, "tracked"), "agent\n")
		testGit(t, work, "commit", "-qam", "agent")
		testGit(t, r.Top, "worktree", "add", "-q", "-b", "elsewhere", filepath.Join(t.TempDir(), "wt"))
		testGit(t, r.Top, "branch", "diverged", testGit(t, r.Top, "commit-tree", "-m", "unrelated", "HEAD^{tree}"))
		for _, branch := range []string{"elsewhere", "diverged"} {
			before := testGit(t, r.Top, "rev-parse", "refs/heads/"+branch)
			var err error
			fetch(func() { _, _, err = r.FetchBranch(context.Background(), gitdir, branch, false) })
			if err == nil {
				t.Errorf("fetch into %s succeeded", branch)
			}
			if got := testGit(t, r.Top, "rev-parse", "refs/heads/"+branch); got != before {
				t.Errorf("%s moved to %s", branch, got)
			}
		}
	})
}
