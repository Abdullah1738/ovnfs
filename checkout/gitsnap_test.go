package checkout

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
)

// snapGit snapshots a checkout and returns the snapshot's id and stored git tree.
func (f *fixture) snapGit(checkout string) (string, string) {
	f.t.Helper()
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: checkout, Wait: true})
	if err != nil {
		f.t.Fatal(err)
	}
	f.reg.mu.RLock()
	defer f.reg.mu.RUnlock()
	return reply.Snapshot.ID, string(f.reg.snapshots[reply.Snapshot.ID].git)
}

// TestGitSnapshotsStoreGitdir checks that snapshots keep a checkout's git directory as a
// stored tree (shared while unchanged, never copied per snapshot), and that checkouts
// from them, including forks, start from exactly that git state.
func TestGitSnapshotsStoreGitdir(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	f.newCheckout(dir, "app")
	gitdir := f.reg.gitdirPath("app")
	initial := gitRun(t, dir, "--git-dir", gitdir, "rev-parse", "HEAD")
	first, firstTree := f.snapGit("app")
	_, secondTree := f.snapGit("app")
	if firstTree == "" || firstTree != secondTree {
		t.Fatalf("unchanged git directory stored as %q then %q", firstTree, secondTree)
	}
	gitRun(t, dir, "--git-dir", gitdir, "commit", "--allow-empty", "-qm", "agent")
	agent := gitRun(t, dir, "--git-dir", gitdir, "rev-parse", "HEAD")
	write(t, filepath.Join(gitdir, "index.lock"), "")
	_, thirdTree := f.snapGit("app")
	if err := os.Remove(filepath.Join(gitdir, "index.lock")); err != nil {
		t.Fatal(err)
	}
	if thirdTree == secondTree {
		t.Fatal("commit in the checkout did not change its stored git directory")
	}
	if entries, err := os.ReadDir(filepath.Join(f.state, "snapshots")); err != nil || len(entries) != 0 {
		t.Fatalf("snapshot directories = %v, %v", entries, err)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: first, Name: "old"}); err != nil {
		t.Fatal(err)
	}
	old := f.reg.gitdirPath("old")
	if got := gitRun(t, dir, "--git-dir", old, "rev-parse", "HEAD"); got != initial {
		t.Fatalf("checkout from first snapshot at %s, want %s", got, initial)
	}
	if got := gitRun(t, dir, "--git-dir", old, "ls-files"); !strings.Contains(got, "tracked.txt") {
		t.Fatalf("index from first snapshot lists %q", got)
	}
	if got := gitRun(t, dir, "--git-dir", old, "config", "--get", "ovnfs.checkout"); got != "old" {
		t.Fatalf("marker = %q", got)
	}
	fork, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	child := f.reg.gitdirPath("child")
	if got := gitRun(t, dir, "--git-dir", child, "cat-file", "-t", "HEAD"); got != "commit" {
		t.Fatalf("fork cannot read its commit: %q", got)
	}
	if got := gitRun(t, dir, "--git-dir", child, "rev-parse", "HEAD"); got != agent {
		t.Fatalf("fork at %s, want %s", got, agent)
	}
	if _, err := os.Lstat(filepath.Join(child, "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("fork inherited a lock file: %v", err)
	}
	if fork.Checkout.Name != "child" {
		t.Fatalf("fork = %+v", fork)
	}
	if entries, err := os.ReadDir(filepath.Join(f.state, "git")); err != nil || len(entries) != 3 {
		t.Fatalf("git directories = %v, %v (want one per checkout, no leftovers)", entries, err)
	}
}

// TestGitSnapshotTreesAreGCRoots sweeps every object no root reaches and checks a
// snapshot's git directory still materializes afterwards.
func TestGitSnapshotTreesAreGCRoots(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	f.newCheckout(dir, "app")
	head := gitRun(t, dir, "--git-dir", f.reg.gitdirPath("app"), "rev-parse", "HEAD")
	id, _ := f.snapGit("app")
	f.reg.mu.RLock()
	roots := f.reg.treeRootsLocked()
	f.reg.mu.RUnlock()
	// A cutoff in the future leaves nothing protected by age or by a recent Build.
	if _, err := f.reg.store.Sweep(roots, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: id, Name: "view"}); err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, dir, "--git-dir", f.reg.gitdirPath("view"), "rev-parse", "HEAD"); got != head {
		t.Fatalf("view at %s, want %s", got, head)
	}
}

// TestLegacySnapshotGitCopy opens a snapshot recorded by a build that copied the whole
// git directory into snapshots/<id>/git, and drops it.
func TestLegacySnapshotGitCopy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	f.newCheckout(dir, "app")
	gitdir := f.reg.gitdirPath("app")
	head := gitRun(t, dir, "--git-dir", gitdir, "rev-parse", "HEAD")
	id, _ := f.snapGit("app")
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(f.state, "snapshots", id)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := git.CopyGitdir(gitdir, filepath.Join(legacy, "git"), "unused"); err != nil {
		t.Fatal(err)
	}
	d := readDisk(t, f.state)
	for i := range d.Snapshots {
		d.Snapshots[i].Git = ""
	}
	if err := writeState(f.state, d); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "--git-dir", gitdir, "commit", "--allow-empty", "-qm", "later")
	f.open()
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: id, Name: "view"}); err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, dir, "--git-dir", f.reg.gitdirPath("view"), "rev-parse", "HEAD"); got != head {
		t.Fatalf("view of legacy snapshot at %s, want %s", got, head)
	}
	if err := f.reg.Drop(ctx, api.DropArgs{Snapshot: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(legacy, "git")); !os.IsNotExist(err) {
		t.Fatalf("dropped legacy copy remains: %v", err)
	}
}

// submoduleRepo returns a repository whose submodule vendor/sub is absorbed into
// its .git/modules, as git submodule add leaves it.
func submoduleRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	work := t.TempDir()
	sub, app := filepath.Join(work, "sub"), filepath.Join(work, "app")
	for _, dir := range []string{sub, app} {
		gitRun(t, work, "init", "-q", "-b", "main", dir)
		gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	}
	gitRun(t, app, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor/sub")
	gitRun(t, app, "commit", "-q", "-m", "add submodule")
	return app
}

// TestSubmoduleGitdirs gives an absorbed submodule a private git directory below
// the checkout's, which the submodule's .git points at; commits there never reach
// the folder's .git/modules, a fork carries them, and rm removes the directory.
func TestSubmoduleGitdirs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	app := submoduleRepo(t)
	orig := filepath.Join(app, ".git", "modules", "vendor", "sub")
	head := gitRun(t, app, "--git-dir", orig, "rev-parse", "HEAD")
	f.newCheckout(app, "app")
	module := filepath.Join(f.reg.gitdirPath("app"), "modules", "vendor", "sub")
	if got, want := f.read("app", "vendor/sub/.git"), "gitdir: "+module+"\n"; got != want {
		t.Fatalf("vendor/sub/.git = %q, want %q", got, want)
	}
	if got := gitRun(t, app, "--git-dir", module, "rev-parse", "HEAD"); got != head {
		t.Fatalf("submodule at %s, want %s", got, head)
	}
	if got := gitRun(t, app, "--git-dir", module, "config", "--get", "ovnfs.checkout"); got != "app" {
		t.Fatalf("submodule marker = %q", got)
	}
	gitRun(t, app, "--git-dir", module, "commit", "--allow-empty", "-qm", "agent")
	agent := gitRun(t, app, "--git-dir", module, "rev-parse", "HEAD")
	if got := gitRun(t, app, "--git-dir", orig, "rev-parse", "HEAD"); got != head {
		t.Fatalf("folder's submodule moved to %s", got)
	}
	if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "child"}); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(f.reg.gitdirPath("child"), "modules", "vendor", "sub")
	if got, want := f.read("child", "vendor/sub/.git"), "gitdir: "+child+"\n"; got != want {
		t.Fatalf("fork's vendor/sub/.git = %q, want %q", got, want)
	}
	if got := gitRun(t, app, "--git-dir", child, "cat-file", "-t", agent); got != "commit" {
		t.Fatalf("fork's submodule cannot read its commit: %q", got)
	}
	if got := gitRun(t, app, "--git-dir", child, "rev-parse", "HEAD"); got != agent {
		t.Fatalf("fork's submodule at %s, want %s", got, agent)
	}
	if got := gitRun(t, app, "--git-dir", child, "config", "--get", "ovnfs.checkout"); got != "child" {
		t.Fatalf("fork's submodule marker = %q", got)
	}
	for _, c := range f.reg.Ls().Checkouts {
		if c.Dirty {
			t.Errorf("checkout %s reports changes; its gitfiles are infrastructure", c.Name)
		}
	}
	for _, name := range []string{"app", "child"} {
		if err := f.reg.Rm(ctx, api.RmArgs{Checkout: name}); err != nil {
			t.Fatal(err)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(f.state, "git")); err != nil || len(entries) != 0 {
		t.Fatalf("git directories after rm = %v, %v", entries, err)
	}
}
