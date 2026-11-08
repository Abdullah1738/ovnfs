package fetch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

func TestUpdateShallowMatchesGit(t *testing.T) {
	g := newGitTest(t)
	src, ids := g.repo(4)
	// git writes the shallow file for two depth-1 fetches, the later of a
	// commit that sorts first.
	gitDir := filepath.Join(t.TempDir(), "git")
	g.git("", "init", "-q", gitDir)
	g.git(gitDir, "fetch", "-q", "--depth=1", "file://"+src, ids[3].String())
	g.git(gitDir, "fetch", "-q", "--depth=1", "file://"+src, ids[1].String())
	want, err := os.ReadFile(filepath.Join(gitDir, ".git", "shallow"))
	if err != nil {
		t.Fatal(err)
	}

	ours := t.TempDir()
	if err := UpdateShallow(ours, []object.ID{ids[3]}, nil); err != nil {
		t.Fatal(err)
	}
	if err := UpdateShallow(ours, []object.ID{ids[1], ids[3]}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(ours, "shallow"))
	if string(got) != string(want) {
		t.Fatalf("shallow file:\n%s\nwant (git):\n%s", got, want)
	}
	if read, err := ReadShallow(ours); err != nil || len(read) != 2 {
		t.Fatalf("ReadShallow = %v, %v", read, err)
	}

	// A held lock is refused and left alone.
	lock := filepath.Join(ours, "shallow.lock")
	os.WriteFile(lock, nil, 0o666)
	if err := UpdateShallow(ours, nil, []object.ID{ids[1]}); !errors.Is(err, ErrShallowLocked) {
		t.Fatalf("with lock held: %v", err)
	}
	os.Remove(lock)

	// Removing every commit removes the file, as git --unshallow does.
	if err := UpdateShallow(ours, nil, []object.ID{ids[1], ids[3]}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ours, "shallow")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shallow file not removed: %v", err)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock left behind: %v", err)
	}
}

func TestLocalFullCopy(t *testing.T) {
	g := newGitTest(t)
	srcDir, ids := g.repo(3)
	src := g.openDB(filepath.Join(srcDir, ".git"))
	dir, gitDir, dst := g.target()
	tip := ids[len(ids)-1]
	res, err := Local(context.Background(), src, dst, []object.ID{tip}, LocalOptions{PackDir: filepath.Join(gitDir, "objects", "pack")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pack != "" || len(res.Shallow) != 0 {
		t.Fatalf("small transfer: %+v", res)
	}
	g.git(dir, "update-ref", "refs/heads/main", tip.String())
	g.verify(dir, gitDir, res, tip)
	if n := g.git(dir, "rev-list", "--count", "refs/heads/main"); n != "3" {
		t.Fatalf("history has %s commits", n)
	}
}

func TestLocalLargeTransferWritesPack(t *testing.T) {
	g := newGitTest(t)
	srcDir, _ := g.repo(1)
	// One commit with 150 files exceeds the loose limit.
	for i := range 150 {
		os.WriteFile(filepath.Join(srcDir, "f"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+i/26))), []byte{byte(i), '\n'}, 0o666)
	}
	g.git(srcDir, "add", "-A")
	g.git(srcDir, "commit", "-q", "-m", "many")
	tip := g.parseID(g.git(srcDir, "rev-parse", "HEAD"))
	dir, gitDir, dst := g.target()
	res, err := Local(context.Background(), g.openDB(filepath.Join(srcDir, ".git")), dst, []object.ID{tip}, LocalOptions{PackDir: filepath.Join(gitDir, "objects", "pack")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pack == "" {
		t.Fatal("no pack written")
	}
	g.git(dir, "update-ref", "refs/heads/main", tip.String())
	g.verify(dir, gitDir, res, tip)
	if loose := g.git(dir, "count-objects"); !strings.HasPrefix(loose, "0 objects") {
		t.Fatalf("loose objects written: %s", loose)
	}
}

func TestLocalDepth(t *testing.T) {
	g := newGitTest(t)
	srcDir, ids := g.repo(4)
	src := g.openDB(filepath.Join(srcDir, ".git"))
	dir, gitDir, dst := g.target()
	opts := LocalOptions{Depth: 1, PackDir: filepath.Join(gitDir, "objects", "pack")}
	res, err := Local(context.Background(), src, dst, []object.ID{ids[2]}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Shallow) != 1 || res.Shallow[0] != ids[2] {
		t.Fatalf("shallow = %v, want [%s]", res.Shallow, ids[2])
	}
	g.git(dir, "update-ref", "refs/heads/main", ids[2].String())
	g.verify(dir, gitDir, res, ids[2])
	if n := g.git(dir, "rev-list", "--count", "refs/heads/main"); n != "1" {
		t.Fatalf("depth 1 history has %s commits", n)
	}

	// Deepening to 3 from the tip unshallows the old boundary.
	opts.Depth = 3
	opts.TargetShallow = res.Shallow
	res, err = Local(context.Background(), src, dst, []object.ID{ids[3]}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(idStrings(res.Shallow), ",") != ids[1].String() || strings.Join(idStrings(res.Unshallow), ",") != ids[2].String() {
		t.Fatalf("deepen: shallow %v unshallow %v", res.Shallow, res.Unshallow)
	}
	g.git(dir, "update-ref", "refs/heads/main", ids[3].String())
	g.verify(dir, gitDir, res, ids[3])
	if n := g.git(dir, "rev-list", "--count", "refs/heads/main"); n != "3" {
		t.Fatalf("depth 3 history has %s commits", n)
	}
}

func TestLocalFromShallowSource(t *testing.T) {
	g := newGitTest(t)
	origin, ids := g.repo(4)
	// The source is itself a depth-2 clone.
	srcDir := filepath.Join(t.TempDir(), "shallow")
	g.git("", "clone", "-q", "--depth=2", "file://"+origin, srcDir)
	srcDB, srcShallow, err := g.openLocal(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	defer srcDB.Close()
	dir, gitDir, dst := g.target()
	res, err := Local(context.Background(), srcDB, dst, []object.ID{ids[3]}, LocalOptions{SourceShallow: srcShallow, PackDir: filepath.Join(gitDir, "objects", "pack")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Shallow) != 1 || res.Shallow[0] != ids[2] {
		t.Fatalf("shallow = %v, want the source's boundary %s", res.Shallow, ids[2])
	}
	g.git(dir, "update-ref", "refs/heads/main", ids[3].String())
	g.verify(dir, gitDir, res, ids[3])
}

func TestLocalIncremental(t *testing.T) {
	g := newGitTest(t)
	srcDir, ids := g.repo(3)
	src := g.openDB(filepath.Join(srcDir, ".git"))
	dir, gitDir, dst := g.target()
	pack := filepath.Join(gitDir, "objects", "pack")
	if _, err := Local(context.Background(), src, dst, []object.ID{ids[1]}, LocalOptions{PackDir: pack}); err != nil {
		t.Fatal(err)
	}
	res, err := Local(context.Background(), src, dst, []object.ID{ids[2]}, LocalOptions{PackDir: pack})
	if err != nil {
		t.Fatal(err)
	}
	// Every commit rewrites all three files: a commit, 4 trees, 3 blobs.
	// Only commit 2's objects may be copied the second time.
	if got := countLoose(t, gitDir) - 8*2; got != 8 {
		t.Fatalf("incremental copy wrote %d objects, want 8", got)
	}
	g.git(dir, "update-ref", "refs/heads/main", ids[2].String())
	g.verify(dir, gitDir, res, ids[2])
}

func countLoose(t *testing.T, gitDir string) int {
	n := 0
	filepath.WalkDir(filepath.Join(gitDir, "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && len(filepath.Base(filepath.Dir(p))) == 2 {
			n++
		}
		return nil
	})
	return n
}

func TestLocalSHA256(t *testing.T) {
	g := newGitTest(t)
	srcDir, ids := g.repo(2, "--object-format=sha256")
	src := g.openDB(filepath.Join(srcDir, ".git"))
	dir, gitDir, dst := g.target("--object-format=sha256")
	res, err := Local(context.Background(), src, dst, ids[1:], LocalOptions{Depth: 1, PackDir: filepath.Join(gitDir, "objects", "pack")})
	if err != nil {
		t.Fatal(err)
	}
	g.git(dir, "update-ref", "refs/heads/main", ids[1].String())
	g.verify(dir, gitDir, res, ids[1])
}
