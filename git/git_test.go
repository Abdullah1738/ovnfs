package git

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testGit runs the real CLI so expectations do not duplicate the implementation.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testRepo creates a repository independent of the developer's configuration.
func testRepo(t *testing.T) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	testGit(t, dir, "init", "-b", "main")
	testGit(t, dir, "config", "user.name", "Test")
	testGit(t, dir, "config", "user.email", "test@localhost")
	testWrite(t, filepath.Join(dir, "tracked"), "original\n")
	testGit(t, dir, "add", "--all")
	testGit(t, dir, "commit", "-m", "initial")
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// testWrite creates parents and writes a fixture file.
func testWrite(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

// testIgnore builds a matcher the way a capture walk does: every directory of root, parents first.
func testIgnore(r *Repo, root string) (*Ignore, error) {
	i, err := NewIgnore(r)
	if err != nil {
		return nil, err
	}
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(root, name)) }
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		return i.Ensure(filepath.ToSlash(rel), read)
	})
	return i, err
}

// TestOpenNotRepo rejects plain directories and opens a nested checkout directory correctly.
func TestOpenNotRepo(t *testing.T) {
	r := testRepo(t)
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("plain directory: %v", err)
	}
	if err := os.Mkdir(filepath.Join(r.Top, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	nested, err := Open(filepath.Join(r.Top, "nested"))
	if err != nil {
		t.Fatal(err)
	}
	if nested.Top != r.Top || nested.CommonDir != r.CommonDir || nested.GitDir != r.GitDir {
		t.Fatalf("nested repo = %#v, want %#v", nested, r)
	}
}

// TestGitdir checks that git inside a checkout sees the captured state and that
// commits made there never reach the user's repository.
func TestGitdir(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	testWrite(t, filepath.Join(r.Top, "tracked"), "staged\n")
	testGit(t, r.Top, "add", "tracked")
	testWrite(t, filepath.Join(r.Top, "tracked"), "unstaged\n")
	state := t.TempDir()
	objects, gitdir, checkout := filepath.Join(state, "objects"), filepath.Join(state, "git"), filepath.Join(state, "checkout")
	if err := r.PrepareObjects(objects); err != nil {
		t.Fatal(err)
	}
	if err := r.MakeGitdir(gitdir, objects, filepath.Join(r.GitDir, "index")); err != nil {
		t.Fatal(err)
	}
	// The checkout holds the user's working file with its mtime, as pins do.
	testWrite(t, filepath.Join(checkout, "tracked"), "unstaged\n")
	info, err := os.Stat(filepath.Join(r.Top, "tracked"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(checkout, "tracked"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := WriteGitfile(checkout, gitdir); err != nil {
		t.Fatal(err)
	}
	if got, want := testGit(t, checkout, "status", "--porcelain"), testGit(t, r.Top, "status", "--porcelain"); got != want {
		t.Fatalf("checkout status %q, user status %q", got, want)
	}
	if got := testGit(t, checkout, "rev-parse", "--show-toplevel"); !strings.HasSuffix(got, "/checkout") {
		t.Fatalf("checkout work tree = %s", got)
	}
	userHead := testGit(t, r.Top, "rev-parse", "HEAD")
	testGit(t, checkout, "commit", "-qam", "agent")
	if testGit(t, r.Top, "rev-parse", "HEAD") != userHead {
		t.Fatal("commit in checkout moved the user's HEAD")
	}
	agent := testGit(t, checkout, "rev-parse", "HEAD")
	if _, err := os.Stat(filepath.Join(r.CommonDir, "objects", agent[:2], agent[2:])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent commit written to the user's objects: %v", err)
	}
	fork := filepath.Join(state, "fork")
	if err := CopyGitdir(gitdir, fork, objects); err != nil {
		t.Fatal(err)
	}
	if err := WriteGitfile(checkout, fork); err != nil {
		t.Fatal(err)
	}
	if testGit(t, checkout, "rev-parse", "HEAD") != agent {
		t.Fatal("copied git directory lost the checkout's HEAD")
	}
	commit, _, err := r.FetchBranch(ctx, fork, "agent", false)
	if err != nil {
		t.Fatal(err)
	}
	if commit != agent || testGit(t, r.Top, "rev-parse", "HEAD") != userHead {
		t.Fatalf("FetchBranch = %s, want %s without moving HEAD", commit, agent)
	}
}

// TestUpstream prefers the configured upstream, falls back to origin's HEAD, and reports none without remotes.
func TestUpstream(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	if ref, commit, err := r.Upstream(ctx); err != nil || ref != "" || commit != "" {
		t.Fatalf("no remote: %q %q %v", ref, commit, err)
	}
	remote := t.TempDir()
	testGit(t, remote, "init", "--bare", "-b", "main")
	testGit(t, r.Top, "remote", "add", "origin", remote)
	testGit(t, r.Top, "push", "-q", "origin", "main")
	testGit(t, r.Top, "remote", "set-head", "origin", "main")
	head := testGit(t, r.Top, "rev-parse", "HEAD")
	if ref, commit, err := r.Upstream(ctx); err != nil || ref != "origin/main" || commit != head {
		t.Fatalf("origin/HEAD fallback: %q %q %v", ref, commit, err)
	}
	testGit(t, r.Top, "push", "-q", "origin", "main:other")
	testGit(t, r.Top, "branch", "-q", "--set-upstream-to", "origin/other")
	testGit(t, r.Top, "commit", "-q", "--allow-empty", "-m", "local")
	ref, commit, err := r.Upstream(ctx)
	if err != nil || ref != "origin/other" || commit != head {
		t.Fatalf("configured upstream: %q %q %v", ref, commit, err)
	}
	if pushed, err := r.Contains(ctx, commit, testGit(t, r.Top, "rev-parse", "HEAD")); err != nil || pushed {
		t.Fatalf("unpushed HEAD reported contained: %v %v", pushed, err)
	}
	if url, err := r.Remote(ctx, ref); err != nil || url != remote {
		t.Fatalf("Remote = %q %v", url, err)
	}
}

// TestMergeFile distinguishes clean merges from conflicts, labels conflict markers,
// and writes no input.
func TestMergeFile(t *testing.T) {
	testRepo(t)
	dir := t.TempDir()
	base, ours, theirs := filepath.Join(dir, "base"), filepath.Join(dir, "ours"), filepath.Join(dir, "theirs")
	testWrite(t, base, "a\nb\nc\n")
	testWrite(t, ours, "A\nb\nc\n")
	testWrite(t, theirs, "a\nb\nC\n")
	labels := Labels{Ours: "folder", Base: "base", Theirs: "checkout work"}
	merged, clean, err := MergeFile(context.Background(), ours, base, theirs, labels)
	if err != nil || !clean || string(merged) != "A\nb\nC\n" {
		t.Fatalf("clean merge = %q %v %v", merged, clean, err)
	}
	testWrite(t, theirs, "X\nb\nc\n")
	merged, clean, err = MergeFile(context.Background(), ours, base, theirs, labels)
	if err != nil || clean {
		t.Fatalf("conflict reported clean: %v", err)
	}
	if want := "<<<<<<< folder\nA\n=======\nX\n>>>>>>> checkout work\nb\nc\n"; string(merged) != want {
		t.Fatalf("conflict = %q, want %q", merged, want)
	}
	if data, _ := os.ReadFile(ours); string(data) != "A\nb\nc\n" {
		t.Fatalf("merge-file wrote ours: %q", data)
	}
}
