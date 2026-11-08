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

// TestCaptureRemembersTrackedContent keeps a clean tracked file's index blob in
// the stat cache, so the next capture takes it without asking git, and stores
// the file once git stops tracking it, since only the index kept its blob in
// the repository.
func TestCaptureRemembersTrackedContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	st, err := store.Open(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "x.stat")
	repo := t.TempDir()
	write(t, filepath.Join(repo, "tracked.txt"), "t")
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(repo, "tracked.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "commit", "-qm", "init")
	time.Sleep(10 * time.Millisecond)
	runGit(t, repo, "update-index", "--refresh")
	first := capture(t, repo, st, cache)
	id := first.Files["tracked.txt"].ID
	if first.Stats.Hashed != 0 || st.Has(id) {
		t.Fatalf("first capture hashed %d files, stored the tracked blob %v", first.Stats.Hashed, st.Has(id))
	}
	if got := loadStatCache(cache, st).Entries["tracked.txt"].OID; got != id {
		t.Fatalf("stat cache holds %q for the tracked file, want %s", got, id)
	}

	runGit(t, repo, "rm", "-q", "--cached", "tracked.txt")
	second := capture(t, repo, st, cache)
	if second.Files["tracked.txt"].ID != id || second.Stats.Hashed != 1 || !st.Has(id) {
		t.Fatalf("untracked file: id %s, hashed %d, stored %v; want it hashed into the store", second.Files["tracked.txt"].ID, second.Stats.Hashed, st.Has(id))
	}
}

// TestCaptureAbsorbedSubmodules leaves the .git of a submodule absorbed into the
// root, and of one absorbed into it, to private git directories placed as git
// nests them, while a nested repository with its own .git directory stays local.
func TestCaptureAbsorbedSubmodules(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	work := t.TempDir()
	for _, name := range []string{"inner", "sub", "app"} {
		dir := filepath.Join(work, name)
		runGit(t, work, "init", "-q", dir)
		runGit(t, dir, "commit", "-q", "--allow-empty", "-m", name)
	}
	runGit(t, filepath.Join(work, "sub"), "-c", "protocol.file.allow=always", "submodule", "add", "-q", filepath.Join(work, "inner"), "lib/inner")
	runGit(t, filepath.Join(work, "sub"), "commit", "-q", "-m", "add inner")
	app := filepath.Join(work, "app")
	runGit(t, app, "-c", "protocol.file.allow=always", "submodule", "add", "-q", filepath.Join(work, "sub"), "vendor/sub")
	runGit(t, app, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init", "--recursive")
	runGit(t, app, "init", "-q", "tools/own")

	res := capture(t, app, nil, "")
	modules := map[string]string{}
	for _, r := range res.Repos {
		modules[r.Path] = r.Module
	}
	want := map[string]string{"": "", "vendor/sub": "modules/vendor/sub", "vendor/sub/lib/inner": "modules/vendor/sub/modules/lib/inner", "tools/own": ""}
	if len(modules) != len(want) {
		t.Fatalf("repositories = %v, want %v", modules, want)
	}
	for p, m := range want {
		if got, ok := modules[p]; !ok || got != m {
			t.Errorf("repository %q module = %q (found %v), want %q", p, got, ok, m)
		}
	}
	for _, p := range []string{".git", "vendor/sub/.git", "vendor/sub/lib/inner/.git"} {
		if class, ok := res.Roots[p]; ok {
			t.Errorf("%s recorded as %s, want it left to a private git directory", p, class)
		}
		if dec, _ := Explain(Dir(app), res.Domains, rules.Set{Rules: rules.Builtins()}, p); dec.Class != rules.Hidden {
			t.Errorf("explain %s = %s, want hidden", p, dec.Class)
		}
	}
	if class := res.Roots["tools/own/.git"]; class != rules.Local {
		t.Errorf("tools/own/.git recorded as %q, want a local copy", class)
	}
}
