package git

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// age moves the times of every file below dir an hour back, so answers derived
// from them are no longer racy and may be remembered.
func age(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		return os.Chtimes(p, old, old)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// withoutGit runs fn where no git binary can start, so only remembered answers serve.
func withoutGit(t *testing.T, fn func()) {
	t.Helper()
	path := os.Getenv("PATH")
	os.Setenv("PATH", "")
	defer os.Setenv("PATH", path)
	fn()
}

// TestRemembersAnswersWhileFilesStay answers Open, Head and configuration reads
// without git while the files they come from stay as they were, and asks git
// again once any of them changes, however soon.
func TestRemembersAnswersWhileFilesStay(t *testing.T) {
	forget()
	t.Cleanup(forget)
	r := testRepo(t)
	ctx := context.Background()
	testGit(t, r.Top, "config", "core.ignorecase", "true")
	testWrite(t, filepath.Join(r.Top, "sub", "file"), "x")
	age(t, r.Top)
	head := testGit(t, r.Top, "rev-parse", "HEAD")
	for range 2 { // the first round remembers, the second must not need git
		sub, err := Open(filepath.Join(r.Top, "sub"))
		if err != nil || sub.Top != r.Top || sub.GitDir != r.GitDir {
			t.Fatalf("Open = %+v, %v; want %+v", sub, err, r)
		}
		if got, err := r.Head(ctx); err != nil || got != head {
			t.Fatalf("Head = %q, %v; want %s", got, err, head)
		}
		if got, err := r.configValue(ctx, "--bool", "--get", "core.ignorecase"); err != nil || got != "true" {
			t.Fatalf("core.ignorecase = %q, %v", got, err)
		}
		if t.Failed() {
			return
		}
		withoutGit(t, func() {
			if _, err := Open(filepath.Join(r.Top, "sub")); err != nil {
				t.Errorf("Open asked git again: %v", err)
			}
			if _, err := r.Head(ctx); err != nil {
				t.Errorf("Head asked git again: %v", err)
			}
			if _, err := r.configValue(ctx, "--bool", "--get", "core.ignorecase"); err != nil {
				t.Errorf("config asked git again: %v", err)
			}
		})
	}

	testGit(t, r.Top, "commit", "-q", "--allow-empty", "-m", "later")
	if got, want := mustHead(t, r), testGit(t, r.Top, "rev-parse", "HEAD"); got != want {
		t.Fatalf("Head after a commit = %s, want %s", got, want)
	}
	testGit(t, r.Top, "config", "core.ignorecase", "false")
	if got, err := r.configValue(ctx, "--bool", "--get", "core.ignorecase"); err != nil || got != "false" {
		t.Fatalf("core.ignorecase after a change = %q, %v", got, err)
	}
	testGit(t, r.Top, "init", "-q", filepath.Join(r.Top, "sub"))
	if sub, err := Open(filepath.Join(r.Top, "sub")); err != nil || sub.Top == r.Top {
		t.Fatalf("Open of a new nested repository = %+v, %v", sub, err)
	}
}

func mustHead(t *testing.T, r *Repo) string {
	t.Helper()
	head, err := r.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return head
}
