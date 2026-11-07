package checkout

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// submoduleRepo returns a repository whose submodule vendor/sub is absorbed into
// its .git/modules, as git submodule add leaves it.
func submoduleRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	isolateGit(t)
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

// export copies a checkout's files to where the mount would show them, so git
// runs inside the checkout as it would through the mount: it finds work trees and
// git directories through the checkout's .git files, and records paths to them.
func (f *fixture) export(checkout string) string {
	f.t.Helper()
	dst := filepath.Join(f.mount, checkout)
	if err := os.RemoveAll(dst); err != nil {
		f.t.Fatal(err)
	}
	s, err := f.reg.Enter(checkout)
	if err != nil {
		f.t.Fatal(err)
	}
	defer s.Done()
	var copyDir func(p string) error
	copyDir = func(p string) error {
		if err := os.MkdirAll(filepath.Join(dst, filepath.FromSlash(p)), 0o755); err != nil {
			return err
		}
		infos, err := s.FS.ReadDir(p)
		if err != nil {
			return err
		}
		for _, info := range infos {
			child := path.Join(p, info.Name())
			host := filepath.Join(dst, filepath.FromSlash(child))
			switch {
			case info.IsDir():
				err = copyDir(child)
			case info.Mode()&fs.ModeSymlink != 0:
				var target string
				if target, err = s.FS.Readlink(child); err == nil {
					err = os.Symlink(target, host)
				}
			default:
				var data []byte
				if data, err = readAll(s, child); err == nil {
					err = os.WriteFile(host, data, info.Mode().Perm())
				}
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := copyDir(""); err != nil {
		f.t.Fatal(err)
	}
	return dst
}

// readAll reads a checkout file through a session.
func readAll(s *Session, p string) ([]byte, error) {
	file, err := s.FS.Open(p)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// realDir resolves a directory's symlinks the way git reports paths.
func realDir(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// assertSubmodule checks that git, run in submodule directory p of an exported
// checkout, works there with gitdir as its git directory.
func assertSubmodule(t *testing.T, exported, p, gitdir string) {
	t.Helper()
	dir := filepath.Join(exported, filepath.FromSlash(p))
	if got, want := gitRun(t, dir, "rev-parse", "--absolute-git-dir"), realDir(t, gitdir); got != want {
		t.Errorf("%s: git directory %s, want %s", p, got, want)
	}
	if got, want := gitRun(t, dir, "rev-parse", "--show-toplevel"), realDir(t, dir); got != want {
		t.Errorf("%s: work tree %s, want %s", p, got, want)
	}
}

// TestSubmoduleGitdirs gives an absorbed submodule a private git directory below
// the checkout's, which git in the submodule's directory uses; commits there never
// reach the folder's .git/modules, a fork carries them, and rm removes them.
func TestSubmoduleGitdirs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	app := submoduleRepo(t)
	orig := filepath.Join(app, ".git", "modules", "vendor", "sub")
	head := gitRun(t, app, "--git-dir", orig, "rev-parse", "HEAD")
	f.newCheckout(app, "app")
	module := filepath.Join(f.reg.gitdirPath("app"), "modules", "vendor", "sub")
	exported := f.export("app")
	assertSubmodule(t, exported, "vendor/sub", module)
	if got := gitRun(t, exported, "status", "--porcelain"); got != "" {
		t.Fatalf("git status in the checkout = %q", got)
	}
	sub := filepath.Join(exported, "vendor", "sub")
	if got := gitRun(t, sub, "rev-parse", "HEAD"); got != head {
		t.Fatalf("submodule at %s, want %s", got, head)
	}
	if got := gitRun(t, sub, "config", "--get", "ovnfs.checkout"); got != "app" {
		t.Fatalf("submodule marker = %q", got)
	}
	gitRun(t, sub, "commit", "--allow-empty", "-qm", "agent")
	agent := gitRun(t, sub, "rev-parse", "HEAD")
	if got := gitRun(t, app, "--git-dir", orig, "rev-parse", "HEAD"); got != head {
		t.Fatalf("folder's submodule moved to %s", got)
	}
	if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "child"}); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(f.reg.gitdirPath("child"), "modules", "vendor", "sub")
	childExported := f.export("child")
	assertSubmodule(t, childExported, "vendor/sub", child)
	if got := gitRun(t, filepath.Join(childExported, "vendor", "sub"), "rev-parse", "HEAD"); got != agent {
		t.Fatalf("fork's submodule at %s, want %s", got, agent)
	}
	if got := gitRun(t, filepath.Join(childExported, "vendor", "sub"), "config", "--get", "ovnfs.checkout"); got != "child" {
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

// TestSubmoduleMovedInFork rebinds a submodule moved in the checkout to the fork's
// own private git directory, so the fork outlives its parent.
func TestSubmoduleMovedInFork(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.newCheckout(submoduleRepo(t), "app")
	s, err := f.reg.Enter("app")
	if err != nil {
		t.Fatal(err)
	}
	err = s.FS.Rename("vendor/sub", "vendor/moved")
	s.Done()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "child"}); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(f.reg.gitdirPath("child"), "modules", "vendor", "sub")
	if got, want := f.read("child", "vendor/moved/.git"), "gitdir: "+child+"\n"; got != want {
		t.Fatalf("fork's vendor/moved/.git = %q, want %q", got, want)
	}
	if err := f.reg.Rm(ctx, api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	assertSubmodule(t, f.export("child"), "vendor/moved", child)
}

// TestSubmoduleReconnectedInFork forks a checkout whose submodule git deinitialized
// and initialized again, which leaves a relative gitfile and a core.worktree that
// both lead into the parent; the fork's submodule is its own.
func TestSubmoduleReconnectedInFork(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.newCheckout(submoduleRepo(t), "app")
	exported := f.export("app")
	gitRun(t, exported, "submodule", "deinit", "-q", "-f", "vendor/sub")
	gitRun(t, exported, "submodule", "update", "-q", "--init")
	gitfile, err := os.ReadFile(filepath.Join(exported, "vendor", "sub", ".git"))
	if err != nil {
		t.Fatal(err)
	}
	f.put("app", "vendor/sub/.git", string(gitfile))
	module := filepath.Join(f.reg.gitdirPath("app"), "modules", "vendor", "sub")
	if gitRun(t, exported, "config", "--file", filepath.Join(module, "config"), "--get", "core.worktree") == "" {
		t.Fatal("git recorded no work tree; the scenario needs one")
	}
	if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "child"}); err != nil {
		t.Fatal(err)
	}
	assertSubmodule(t, f.export("child"), "vendor/sub", filepath.Join(f.reg.gitdirPath("child"), "modules", "vendor", "sub"))
}

// TestSubmoduleDeinitStays keeps a submodule whose .git was removed (as git
// submodule deinit leaves it) without one in later snapshots and forks.
func TestSubmoduleDeinitStays(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.newCheckout(submoduleRepo(t), "app")
	f.remove("app", "vendor/sub/.git")
	if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "early"}); err != nil {
		t.Fatal(err)
	}
	f.put("app", "note.txt", "a real change, so the snapshot freezes a layer\n")
	f.snapGit("app")
	if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "late"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app", "early", "late"} {
		if got := f.read(name, "vendor/sub/.git"); got != "<absent>" {
			t.Errorf("%s: vendor/sub/.git = %q, want it still removed", name, got)
		}
	}
}

// TestSubmoduleDirMetadataIsAChange snapshots a chmod or touch of a directory
// copied up to hold a submodule's gitfile as a change of its own.
func TestSubmoduleDirMetadataIsAChange(t *testing.T) {
	f := newFixture(t)
	app := submoduleRepo(t)
	for name, change := range map[string]func(s *Session) error{
		"chmod": func(s *Session) error { return s.FS.Chmod("vendor/sub", 0o700) },
		"touch": func(s *Session) error {
			at := time.Now().Add(-time.Hour)
			return s.FS.Chtimes("vendor/sub", at, at)
		},
	} {
		f.newCheckout(app, name)
		if f.dirty(name) {
			t.Fatalf("%s: a fresh checkout reports changes", name)
		}
		s, err := f.reg.Enter(name)
		if err != nil {
			t.Fatal(err)
		}
		err = change(s)
		s.Done()
		if err != nil {
			t.Fatal(err)
		}
		if !f.dirty(name) {
			t.Errorf("%s of vendor/sub reports no change", name)
		}
		id, _ := f.snapGit(name)
		f.reg.mu.RLock()
		layer := f.reg.snapshots[id].layer
		f.reg.mu.RUnlock()
		if layer == "" {
			t.Errorf("%s of vendor/sub froze no layer", name)
		}
	}
}

// dirty reports whether a checkout lists changes.
func (f *fixture) dirty(checkout string) bool {
	f.t.Helper()
	for _, c := range f.reg.Ls().Checkouts {
		if c.Name == checkout {
			return c.Dirty
		}
	}
	f.t.Fatalf("no checkout %s", checkout)
	return false
}

// TestGitdirKeepsLockNamedModules stores a submodule git directory whose name
// ends in .lock, while lock files of in-flight git commands stay out.
func TestGitdirKeepsLockNamedModules(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(dir, "index.lock"), "")
	write(t, filepath.Join(dir, "modules", "dep.lock", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(dir, "modules", "dep.lock", "config.lock"), "")
	tree, err := saveGitdir(f.reg.store, dir, &gitCache{})
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "git")
	if err := materializeGitdir(f.reg.store, tree, dst, func(string) string { return t.TempDir() }, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "modules", "dep.lock", "HEAD")); err != nil {
		t.Errorf("module dep.lock not restored: %v", err)
	}
	for _, lock := range []string{"index.lock", "modules/dep.lock/config.lock"} {
		if _, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(lock))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("lock file %s restored: %v", lock, err)
		}
	}
}
