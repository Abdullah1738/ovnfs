package checkout

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/store"
)

func revisionRepo(t *testing.T) string {
	t.Helper()
	dir := folder(t, map[string]string{
		"tracked.txt": "original\n",
		"other.txt":   "other\n",
		".gitignore":  "build/\n*.log\nnode_modules/kept/\n",
	})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "initial")
	return dir
}

func TestNewRevisionDirtyFolder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, "tracked.txt"), "user's dirty edit\n")
	write(t, filepath.Join(dir, "new.txt"), "untracked\n")
	write(t, filepath.Join(dir, "build/out.o"), "kept build\n")
	write(t, filepath.Join(dir, "debug.log"), "kept ignored file\n")
	// Neither a dirty ignore file nor a nonignored built-in cache directory
	// should leak untracked files into the revision checkout.
	write(t, filepath.Join(dir, ".gitignore"), "build/\nnew.txt\n")
	write(t, filepath.Join(dir, "node_modules/not-ignored"), "remove\n")
	write(t, filepath.Join(dir, "node_modules/kept/cache"), "nested ignored\n")
	reply, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "revision", Rev: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		"tracked.txt": "original\n", "new.txt": "<absent>",
		"build/out.o": "kept build\n", "debug.log": "kept ignored file\n",
		"node_modules/not-ignored": "<absent>",
		"node_modules/kept/cache":  "nested ignored\n",
	} {
		if got := f.read(reply.Checkout.Name, p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	// A small materialized view exercises the private index with real git,
	// without requiring a kernel mount in package tests.
	work := t.TempDir()
	for _, p := range []string{".gitignore", "tracked.txt", "other.txt"} {
		write(t, filepath.Join(work, p), f.read("revision", p))
	}
	if got := gitRun(t, work, "--git-dir", f.reg.gitdirPath("revision"), "--work-tree", work, "status", "--porcelain"); got != "" {
		t.Fatalf("revision status = %q", got)
	}
	assertDiff := func(want string) {
		t.Helper()
		diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "revision"})
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, change := range diff.Changes {
			paths = append(paths, change.Path)
		}
		if got := strings.Join(paths, " "); got != want {
			t.Fatalf("diff = %q, want %q", got, want)
		}
	}
	assertDiff("")
	f.put("revision", "other.txt", "agent edit\n")
	assertDiff("other.txt")
	for _, dry := range []bool{true, false} {
		applied, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "revision", DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if len(applied.Applied) != 1 || applied.Applied[0].Path != "other.txt" || len(applied.Conflicts) != 0 {
			t.Fatalf("apply dry=%t = %+v", dry, applied)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "tracked.txt")); err != nil || string(got) != "user's dirty edit\n" {
			t.Fatalf("source dirty edit = %q, %v", got, err)
		}
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	assertDiff("other.txt")
}

// TestEditKeepsExecutableMode snapshots an edited executable from a revision as
// still executable, on hosts whose upper files cannot hold mode bits too.
func TestEditKeepsExecutableMode(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, "run.sh"), "echo old\n")
	gitRun(t, dir, "add", "run.sh")
	gitRun(t, dir, "update-index", "--chmod=+x", "run.sh")
	gitRun(t, dir, "commit", "-qm", "executable")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: "exec", Rev: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	f.put("exec", "run.sh", "echo new\n")
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "exec", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.reg.store.Flatten(store.OID(reply.Snapshot.Tree))
	if err != nil {
		t.Fatal(err)
	}
	if got := files["run.sh"].Mode; got != store.ModeExec {
		t.Fatalf("edited executable snapshots as mode %o", got)
	}
}

func TestRevisionHEADAttachment(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	commit := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "tag", "release")
	for _, tt := range []struct{ rev, head string }{
		{"main", "ref: refs/heads/main"},
		{"refs/heads/main", "ref: refs/heads/main"},
		{commit, commit}, {"HEAD", commit}, {"release", commit},
	} {
		t.Run(tt.rev, func(t *testing.T) {
			reply, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Rev: tt.rev})
			if err != nil {
				t.Fatal(err)
			}
			head, err := os.ReadFile(filepath.Join(f.reg.gitdirPath(reply.Checkout.Name), "HEAD"))
			if err != nil || strings.TrimSpace(string(head)) != tt.head {
				t.Fatalf("HEAD = %q, %v; want %q", head, err, tt.head)
			}
		})
	}
}

func TestNewHistoricalRevision(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	commit := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "mv", "tracked.txt", "renamed.txt")
	write(t, filepath.Join(dir, "other.txt"), "new commit\n")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "different tree")
	write(t, filepath.Join(dir, "staged.txt"), "staged addition\n")
	gitRun(t, dir, "add", "staged.txt")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: "historical", Rev: commit}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		"tracked.txt": "original\n", "other.txt": "other\n",
		"renamed.txt": "<absent>", "staged.txt": "<absent>",
	} {
		if got := f.read("historical", p); got != want {
			t.Fatalf("%s = %q, want %q", p, got, want)
		}
	}
	if diff, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "historical"}); err != nil || len(diff.Changes) != 0 {
		t.Fatalf("historical diff = %+v, %v", diff, err)
	}
	if got := strings.Join(f.snapFiles("historical"), " "); got != ".gitignore other.txt tracked.txt" {
		t.Fatalf("historical snapshot = %s", got)
	}
}

func TestRevisionErrorsLeaveNoCheckout(t *testing.T) {
	f := newFixture(t)
	repo := revisionRepo(t)
	for _, args := range []api.NewArgs{
		{Dir: repo, Name: "bad", Rev: "does-not-exist"},
		{Dir: t.TempDir(), Name: "bad", Rev: "HEAD"},
		{Snapshot: "missing", Name: "bad", Rev: "HEAD"},
	} {
		if _, err := f.reg.New(context.Background(), args); err == nil {
			t.Fatalf("new %+v unexpectedly succeeded", args)
		}
		if len(f.reg.checkouts) != 0 || len(f.reg.bases) != 0 || len(f.reg.reserved) != 0 || len(f.reg.intents) != 0 {
			t.Fatal("failed revision creation left registry state")
		}
		for _, sub := range []string{"git", "uppers", "bases"} {
			entries, err := os.ReadDir(filepath.Join(f.state, sub))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed creation left %s: %v, %v", sub, entries, err)
			}
		}
	}
}

func TestCheckoutMarkersAndMigration(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	f.newCheckout(dir, "parent")
	fork, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "parent", Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: fork.Snapshot.ID, Name: "snapshot"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"parent", "child", "snapshot"} {
		if got := gitRun(t, dir, "--git-dir", f.reg.gitdirPath(name), "config", "--local", "--get", "ovnfs.checkout"); got != name {
			t.Fatalf("%s marker = %q", name, got)
		}
	}
	config, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil || strings.Contains(string(config), "[ovnfs]") {
		t.Fatalf("source config changed: %s, %v", config, err)
	}
	gitRun(t, dir, "--git-dir", f.reg.gitdirPath("parent"), "config", "--local", "--unset", "ovnfs.checkout")
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if got := gitRun(t, dir, "--git-dir", f.reg.gitdirPath("parent"), "config", "--local", "--get", "ovnfs.checkout"); got != "parent" {
		t.Fatalf("reopened checkout marker = %q", got)
	}
}
