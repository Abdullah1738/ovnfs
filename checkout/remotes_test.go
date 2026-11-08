package checkout

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
)

// TestGitRemotes follows a repository's checkout remotes through creates, a
// fetch under fetch.pruneTags, a removal, and the setting turned off at restart.
func TestGitRemotes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.reg.Close()
	f.gitRemotes = true
	f.open()
	dir := folder(t, map[string]string{"a.go": "a\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "initial")
	f.newCheckout(dir, "one")
	f.newCheckout(dir, "two")
	if got := gitRun(t, dir, "remote"); got != "ovnfs/one\novnfs/two" {
		t.Fatalf("remotes = %q", got)
	}
	// A checkout's config is a copy of the folder's: it must not reach the others.
	if got := gitRun(t, dir, "--git-dir", f.gitdir("two"), "remote"); got != "" {
		t.Fatalf("remotes inside checkout two = %q", got)
	}

	gitRun(t, dir, "--git-dir", f.gitdir("one"), "commit", "--allow-empty", "-qm", "in one")
	gitRun(t, dir, "--git-dir", f.gitdir("one"), "tag", "from-one")
	want := gitRun(t, dir, "--git-dir", f.gitdir("one"), "rev-parse", "HEAD")
	gitRun(t, dir, "tag", "after-capture")
	gitRun(t, dir, "config", "fetch.prune", "true")
	gitRun(t, dir, "config", "fetch.pruneTags", "true")
	gitRun(t, dir, "fetch", "-q", "--all")
	if got := gitRun(t, dir, "rev-parse", "ovnfs/one"); got != want {
		t.Fatalf("ovnfs/one = %s, want %s", got, want)
	}
	if got := gitRun(t, dir, "tag"); got != "after-capture" {
		t.Fatalf("tags after fetch = %q, want the folder's own, none fetched or pruned", got)
	}

	if err := f.reg.Rm(ctx, api.RmArgs{Checkout: "one"}); err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, dir, "remote"); got != "ovnfs/two" {
		t.Fatalf("remotes after rm = %q", got)
	}
	if got := gitRun(t, dir, "for-each-ref", "--format=%(refname)", "refs/remotes/ovnfs/"); got != "refs/remotes/ovnfs/two/HEAD" {
		t.Fatalf("tracking refs after rm = %q", got)
	}

	f.reg.Close()
	f.gitRemotes = false
	f.open()
	if got := gitRun(t, dir, "remote"); got != "" {
		t.Fatalf("remotes with the setting off = %q", got)
	}
	if got := gitRun(t, dir, "for-each-ref", "refs/remotes/"); got != "" {
		t.Fatalf("tracking refs with the setting off = %q", got)
	}
	config, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), git.RemotesFile) {
		t.Fatalf("config still includes %s:\n%s", git.RemotesFile, config)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", git.RemotesFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s left behind: %v", git.RemotesFile, err)
	}
}
