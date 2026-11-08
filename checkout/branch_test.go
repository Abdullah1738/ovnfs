package checkout

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

func TestApplyCheckedOutBranch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.go": "base a\n", "b.go": "base b\n", "c.go": "base c\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "initial")
	for _, path := range []string{"a.go", "b.go", "c.go"} {
		write(t, filepath.Join(dir, path), "dirty "+path+"\n")
	}
	gitRun(t, dir, "add", "c.go")
	if _, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "land", Rev: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	f.put("land", "a.go", "dirty a.go\n")
	// Materialize only the git command's work tree; the registry's checkout uses
	// the same private index and object store without requiring a kernel mount.
	work := t.TempDir()
	for _, path := range []string{"a.go", "b.go", "c.go"} {
		write(t, filepath.Join(work, path), f.read("land", path))
	}
	gitdir := f.gitdir("land")
	gitRun(t, work, "--git-dir", gitdir, "--work-tree", work, "add", "a.go")
	gitRun(t, work, "--git-dir", gitdir, "--work-tree", work, "commit", "-qm", "land a")
	want := gitRun(t, work, "--git-dir", gitdir, "rev-parse", "HEAD")
	old := gitRun(t, dir, "rev-parse", "HEAD")
	indexPath := filepath.Join(dir, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		reply, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "land", Branch: "main", DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Branch != "main" || reply.Commit != want || len(reply.Applied) != 1 || reply.Applied[0] != (api.Change{Path: "a.go", Status: "M"}) {
			t.Fatalf("apply dry=%t: %+v", dry, reply)
		}
		if dry {
			after, err := os.ReadFile(indexPath)
			if err != nil || !bytes.Equal(before, after) || gitRun(t, dir, "rev-parse", "HEAD") != old {
				t.Fatalf("dry run changed source index or ref: %v", err)
			}
		}
	}
	if got := gitRun(t, dir, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD = %s, want %s", got, want)
	}
	if got := gitRun(t, dir, "status", "--porcelain"); got != "M b.go\nM  c.go" {
		t.Fatalf("status = %q", got)
	}
	for _, path := range []string{"a.go", "b.go", "c.go"} {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || string(data) != "dirty "+path+"\n" {
			t.Fatalf("source %s = %q, %v", path, data, err)
		}
	}
}

func TestApplyBranchConflictCode(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: "land", Rev: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "commit", "-qm", "source advanced", "--allow-empty")
	_, err := f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: "land", Branch: "main"})
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != "conflict" {
		t.Fatalf("apply error = %v", err)
	}
}
