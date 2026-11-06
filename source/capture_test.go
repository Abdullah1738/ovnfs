package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
)

// write creates a fixture file with parents.
func write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// capture runs one capture of dir with the built-in rules.
func capture(t *testing.T, dir string, st *store.Store, cache string) Result {
	t.Helper()
	root, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Capture(context.Background(), Options{Root: root, Rules: rules.Set{Rules: rules.Builtins()}, Store: st, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// runGit runs the real CLI in dir.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestCaptureHashesConvertedContent takes a clean tracked file's index blob id
// only when it is the work-tree bytes: end-of-line conversion that changed bytes
// and filter drivers are hashed, files that conversion left alone are not.
func TestCaptureHashesConvertedContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	repo := t.TempDir()
	sub := filepath.Join(repo, "sub")
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "core.autocrlf", "false")
	runGit(t, repo, "config", "filter.upper.clean", "tr A-Z a-z")
	runGit(t, repo, "config", "filter.upper.smudge", "tr a-z A-Z")
	write(t, filepath.Join(repo, ".gitattributes"), "*.crlf text eol=crlf\n*.up filter=upper\n")
	write(t, filepath.Join(repo, "lines.crlf"), "one\ntwo\n")
	write(t, filepath.Join(repo, "single.crlf"), "no line end")
	write(t, filepath.Join(repo, "case.up"), "abc\n")
	write(t, filepath.Join(repo, "plain.txt"), "plain\n")
	runGit(t, repo, "add", ".gitattributes", "lines.crlf", "single.crlf", "case.up", "plain.txt")
	runGit(t, repo, "commit", "-qm", "init")
	// A nested repository converts by its own configuration.
	write(t, filepath.Join(sub, "auto.txt"), "x\ny\n")
	write(t, filepath.Join(sub, "single.txt"), "z")
	runGit(t, sub, "init", "-q")
	runGit(t, sub, "config", "core.autocrlf", "true")
	runGit(t, sub, "add", "auto.txt", "single.txt")
	runGit(t, sub, "commit", "-qm", "init")
	// Check out again so the work tree holds converted bytes, then make every
	// entry stat-clean: older than the index that records it.
	old := time.Now().Add(-time.Minute)
	for dir, names := range map[string][]string{repo: {".gitattributes", "lines.crlf", "single.crlf", "case.up", "plain.txt"}, sub: {"auto.txt", "single.txt"}} {
		for _, n := range names {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				t.Fatal(err)
			}
		}
		runGit(t, dir, append([]string{"checkout", "--"}, names...)...)
		for _, n := range names {
			if err := os.Chtimes(filepath.Join(dir, n), old, old); err != nil {
				t.Fatal(err)
			}
		}
		runGit(t, dir, "update-index", "--refresh")
	}

	res := capture(t, repo, nil, "")
	for p, want := range map[string]string{
		"lines.crlf":     "one\r\ntwo\r\n",
		"single.crlf":    "no line end",
		"case.up":        "ABC\n",
		"plain.txt":      "plain\n",
		"sub/auto.txt":   "x\r\ny\r\n",
		"sub/single.txt": "z",
	} {
		if got := res.Files[p].ID; got != store.HashBytes("blob", []byte(want)) {
			t.Errorf("%s captured as %s, want the id of %q", p, got, want)
		}
	}
	if res.Stats.Hashed != 3 {
		t.Errorf("hashed %d files, want only the 3 that git converted", res.Stats.Hashed)
	}
}

// TestCaptureReusesKnownContent hashes only what changed since the last capture,
// and takes clean tracked files' ids from the git index without reading them.
func TestCaptureReusesKnownContent(t *testing.T) {
	st, err := store.Open(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "x.stat")
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "a")
	write(t, filepath.Join(dir, "b.txt"), "b")
	old := time.Now().Add(-time.Minute)
	for _, n := range []string{"a.txt", "b.txt"} {
		if err := os.Chtimes(filepath.Join(dir, n), old, old); err != nil {
			t.Fatal(err)
		}
	}
	first := capture(t, dir, st, cache)
	if first.Stats.Hashed != 2 {
		t.Fatalf("first capture hashed %d", first.Stats.Hashed)
	}
	second := capture(t, dir, st, cache)
	if second.Stats.Hashed != 0 || second.Tree != first.Tree {
		t.Fatalf("second capture hashed %d, tree %s vs %s", second.Stats.Hashed, second.Tree, first.Tree)
	}
	write(t, filepath.Join(dir, "b.txt"), "changed")
	third := capture(t, dir, st, cache)
	if third.Stats.Hashed != 1 || third.Tree == first.Tree {
		t.Fatalf("third capture hashed %d", third.Stats.Hashed)
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	write(t, filepath.Join(repo, "tracked.txt"), "t")
	write(t, filepath.Join(repo, "nested", "inner.txt"), "i")
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "commit", "-qm", "init")
	runGit(t, filepath.Join(repo, "nested"), "init", "-q")
	// The index's racy-clean rule needs the index to be newer than the file.
	time.Sleep(10 * time.Millisecond)
	runGit(t, repo, "update-index", "--refresh")
	res := capture(t, repo, st, "")
	if res.Stats.Hashed != 1 {
		t.Fatalf("repo capture hashed %d files, want only the untracked nested file", res.Stats.Hashed)
	}
	if len(res.Repos) != 2 || res.Repos[1].Path != "nested" || res.Roots["nested/.git"] != rules.Local {
		t.Fatalf("repos %+v, nested .git class %q", res.Repos, res.Roots["nested/.git"])
	}
}
