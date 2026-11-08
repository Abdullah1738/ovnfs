package repo

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
)

func TestOpen(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	main := filepath.Join(base, "main")
	newRepo(t, main)
	wt := filepath.Join(base, "wt")
	git(t, main, "worktree", "add", "-q", "-b", "w", wt)
	mirror := filepath.Join(base, "mirror.git")
	git(t, base, "clone", "-q", "--mirror", main, mirror)

	// A private git directory like a checkout's: core.bare=false, objects
	// a symlink to a shared object directory.
	priv := filepath.Join(base, "priv")
	mkdirs(t, filepath.Join(priv, "refs", "heads"))
	writeFile(t, filepath.Join(priv, "HEAD"), git(t, main, "rev-parse", "HEAD")+"\n")
	writeFile(t, filepath.Join(priv, "config"), "[core]\n\trepositoryformatversion = 0\n\tbare = false\n")
	if err := os.Symlink(filepath.Join(main, ".git", "objects"), filepath.Join(priv, "objects")); err != nil {
		t.Fatal(err)
	}

	phys := func(p string) string {
		r, err := realPath(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, tc := range []struct {
		path string
		top  string
		bare bool
	}{
		{mirror, "", true},
		{filepath.Join(main, ".git"), phys(main), false},
		{filepath.Join(wt, ".git"), phys(wt), false},
		{filepath.Join(main, ".git", "worktrees", "wt"), phys(wt), false},
		{priv, "", false},
	} {
		t.Run(strings.TrimPrefix(tc.path, base), func(t *testing.T) {
			r, err := Open(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			out := strings.Split(git(t, base, "--git-dir="+tc.path, "rev-parse", "--path-format=absolute",
				"--git-common-dir", "--git-dir", "--is-bare-repository"), "\n")
			if r.CommonDir != out[0] || r.GitDir != out[1] {
				t.Errorf("Open: common %q gitdir %q; git %q", r.CommonDir, r.GitDir, out)
			}
			if r.Top != tc.top || r.Bare != tc.bare || (out[2] == "true") != tc.bare {
				t.Errorf("Open: top %q bare %v; want %q %v (git bare %s)", r.Top, r.Bare, tc.top, tc.bare, out[2])
			}
			head, err := r.Refs().Resolve("HEAD")
			if err != nil {
				t.Fatal(err)
			}
			objs, err := r.OpenObjects()
			if err != nil {
				t.Fatal(err)
			}
			defer objs.Close()
			if k, _, err := objs.Header(head.ID); err != nil || k != object.Commit {
				t.Fatalf("HEAD %s: %v %v", head.ID, k, err)
			}
		})
	}
	for _, p := range []string{main, filepath.Join(base, "missing"), filepath.Join(main, ".git", "objects")} {
		if _, err := Open(p); err == nil {
			t.Errorf("Open(%s) succeeded", p)
		}
	}
	if _, err := Open(main); !errors.Is(err, ErrNotRepository) {
		t.Errorf("Open(work tree) = %v", err)
	}
}

func TestWorktrees(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	main := filepath.Join(base, "main")
	newRepo(t, main)
	git(t, main, "worktree", "add", "-q", "-b", "one", filepath.Join(base, "one"))
	git(t, main, "worktree", "add", "-q", "--detach", filepath.Join(base, "two"))
	r, err := Discover(filepath.Join(base, "one"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := r.Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	var got, want []string
	for _, w := range list {
		head, err := w.Refs().Symbolic("HEAD")
		if errors.Is(err, refs.ErrNotSymbolic) {
			head, err = "detached", nil
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, w.Top+" "+head)
	}
	var top string
	for line := range strings.SplitSeq(git(t, main, "worktree", "list", "--porcelain"), "\n") {
		switch k, v, _ := strings.Cut(line, " "); k {
		case "worktree":
			top = v
		case "branch":
			want = append(want, top+" "+v)
		case "detached":
			want = append(want, top+" detached")
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Worktrees = %q; git %q", got, want)
	}

	bare := filepath.Join(base, "bare.git")
	git(t, base, "clone", "-q", "--bare", main, bare)
	git(t, bare, "worktree", "add", "-q", "-b", "x", filepath.Join(base, "x"))
	br, err := Open(bare)
	if err != nil {
		t.Fatal(err)
	}
	list, err = br.Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || filepath.Base(list[0].Top) != "x" {
		t.Fatalf("bare Worktrees = %+v", list)
	}
}

func TestInitBareMatchesGit(t *testing.T) {
	isolate(t)
	for _, tc := range []struct {
		name   string
		global string
		env    map[string]string
	}{
		{name: "default"},
		{name: "defaultBranch", global: "[init]\n\tdefaultBranch = trunk\n"},
		{name: "sha256-config", global: "[init]\n\tdefaultObjectFormat = sha256\n"},
		{name: "sha256-env", env: map[string]string{"GIT_DEFAULT_HASH": "sha256"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.global != "" {
				global := filepath.Join(t.TempDir(), "global")
				writeFile(t, global, tc.global)
				t.Setenv("GIT_CONFIG_GLOBAL", global)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			base := t.TempDir()
			theirs, ours := filepath.Join(base, "theirs"), filepath.Join(base, "ours")
			git(t, base, "init", "-q", "--bare", theirs)
			r, err := InitBare(ours)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Bare || r.Top != "" {
				t.Fatalf("InitBare: %+v", r)
			}
			if a, b := listTree(t, theirs), listTree(t, ours); !slices.Equal(a, b) {
				t.Fatalf("layout differs:\ngit:  %q\nours: %q", a, b)
			}
			for _, f := range []string{"config", "HEAD", "description", "info/exclude"} {
				a, err := os.ReadFile(filepath.Join(theirs, f))
				if err != nil {
					t.Fatal(err)
				}
				b, err := os.ReadFile(filepath.Join(ours, f))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(a, b) {
					t.Errorf("%s differs:\ngit:\n%s\nours:\n%s", f, a, b)
				}
			}
			git(t, ours, "fsck", "--strict")
			if got := git(t, ours, "rev-parse", "--is-bare-repository"); got != "true" {
				t.Fatalf("git sees bare = %s", got)
			}
			again, err := InitBare(ours)
			if err != nil || again.GitDir != r.GitDir {
				t.Fatalf("InitBare on existing: %+v %v", again, err)
			}
		})
	}
	t.Run("reftable-default-ignored", func(t *testing.T) {
		global := filepath.Join(t.TempDir(), "global")
		writeFile(t, global, "[init]\n\tdefaultRefFormat = reftable\n\tdefaultRefStorageFormat = reftable\n")
		t.Setenv("GIT_CONFIG_GLOBAL", global)
		t.Setenv("GIT_DEFAULT_REF_FORMAT", "reftable")
		t.Setenv("GIT_DEFAULT_REF_STORAGE_FORMAT", "reftable")
		dir := filepath.Join(t.TempDir(), "r")
		r, err := InitBare(dir)
		if err != nil {
			t.Fatal(err)
		}
		if r.RefBackend != refs.BackendFiles {
			t.Fatalf("RefBackend = %q", r.RefBackend)
		}
		if got := git(t, dir, "rev-parse", "--show-ref-format"); got != "files" {
			t.Fatalf("git sees ref format %q", got)
		}
		git(t, dir, "fsck", "--strict")
	})
	t.Run("bad-branch", func(t *testing.T) {
		global := filepath.Join(t.TempDir(), "global")
		writeFile(t, global, "[init]\n\tdefaultBranch = a..b\n")
		t.Setenv("GIT_CONFIG_GLOBAL", global)
		if _, err := InitBare(filepath.Join(t.TempDir(), "r")); err == nil {
			t.Fatal("invalid initial branch accepted")
		}
	})
}

// listTree lists a git directory's entries, without sample hooks.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "hooks" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCommitterMatchesReflog(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "r")
	git(t, filepath.Dir(dir), "init", "-q", dir)
	c := gitInput(t, dir, "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor a <a@b> 0 +0000\ncommitter a <a@b> 0 +0000\n\nx\n",
		"hash-object", "-t", "commit", "-w", "--stdin")
	r, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	check := func(t *testing.T, exactTime bool) {
		t.Helper()
		n++
		ref := "refs/heads/t" + string(rune('a'+n))
		git(t, dir, "update-ref", "-m", "test", ref, c)
		logs, err := r.Refs().Reflog(ref)
		if err != nil || len(logs) != 1 {
			t.Fatalf("reflog: %v %v", logs, err)
		}
		want := logs[0].Committer
		got, err := r.Committer()
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != want.Name || got.Email != want.Email {
			t.Fatalf("Committer = %q <%q>; git wrote %q <%q>", got.Name, got.Email, want.Name, want.Email)
		}
		_, gotOff := got.When.Zone()
		_, wantOff := want.When.Zone()
		d := got.When.Sub(want.When)
		if gotOff != wantOff || exactTime && d != 0 || d < -5*time.Second || d > 5*time.Second {
			t.Fatalf("Committer time %v; git wrote %v", got.When, want.When)
		}
	}
	t.Run("no-config", func(t *testing.T) { check(t, false) })
	t.Run("env", func(t *testing.T) {
		t.Setenv("GIT_COMMITTER_NAME", "  .Foo <x>; ")
		t.Setenv("GIT_COMMITTER_EMAIL", " <a@b>. ")
		t.Setenv("GIT_COMMITTER_DATE", "1700000000 +0130")
		check(t, true)
	})
	t.Run("env-empty", func(t *testing.T) {
		t.Setenv("GIT_COMMITTER_NAME", "")
		t.Setenv("GIT_COMMITTER_EMAIL", "")
		check(t, false)
	})
	for _, date := range []string{"@1700000000 -0730", "2023-11-14T22:13:20+01:00", "2005-04-07 22:13:13",
		"2005-04-07T22:13:13.5Z", "Thu, 07 Apr 2005 22:13:13 +0200", "1700000000", "1700000000 +05"} {
		t.Run("date "+date, func(t *testing.T) {
			t.Setenv("GIT_COMMITTER_DATE", date)
			check(t, true)
		})
	}
	t.Run("bad-date", func(t *testing.T) {
		t.Setenv("GIT_COMMITTER_DATE", "garbage")
		if _, err := r.Committer(); err == nil {
			t.Fatal("garbage date accepted")
		}
	})
	t.Run("config", func(t *testing.T) {
		t.Setenv("EMAIL", "env@example.com")
		check(t, false)
		git(t, dir, "config", "user.name", "User Name")
		git(t, dir, "config", "user.email", "user@example.com")
		check(t, false)
		git(t, dir, "config", "committer.name", "Committer")
		git(t, dir, "config", "committer.email", "")
		check(t, false)
		git(t, dir, "config", "user.email", "")
		check(t, false)
		git(t, dir, "config", "committer.email", "c@example.com")
		t.Setenv("GIT_COMMITTER_NAME", "Env")
		check(t, false)
	})
}

func TestReplaceRefs(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "r")
	git(t, filepath.Dir(dir), "init", "-q", dir)
	writeFile(t, filepath.Join(dir, "a"), "a\n")
	git(t, dir, "add", "a")
	commit(t, dir, "one")
	c1 := git(t, dir, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "b"), "b\n")
	git(t, dir, "add", "b")
	commit(t, dir, "two")
	c2 := git(t, dir, "rev-parse", "HEAD")
	blobA := gitInput(t, dir, "x\n", "hash-object", "-w", "--stdin")
	blobB := gitInput(t, dir, "y\n", "hash-object", "-w", "--stdin")
	blobC := gitInput(t, dir, "z\n", "hash-object", "-w", "--stdin")

	r, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := func(s string) object.ID {
		v, err := object.ParseID(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	compare := func(t *testing.T) {
		t.Helper()
		objs, err := r.OpenObjects()
		if err != nil {
			t.Fatal(err)
		}
		defer objs.Close()
		for _, name := range []string{c1, blobA, blobB} {
			_, data, err := objs.Read(id(name))
			if err != nil {
				t.Fatal(err)
			}
			if want := git(t, dir, "cat-file", "-p", name); strings.TrimSpace(string(data)) != want {
				t.Errorf("Read(%s) = %q; git %q", name, data, want)
			}
		}
		tree, err := objs.Peel(id(c1), object.Tree)
		if err != nil {
			t.Fatal(err)
		}
		if want := git(t, dir, "rev-parse", c1+"^{tree}"); tree.String() != want {
			t.Errorf("Peel(%s, tree) = %s; git %s", c1, tree, want)
		}
		src, err := r.RevSource(objs)
		if err != nil {
			t.Fatal(err)
		}
		got, err := rev.Resolve(&src, c1+"^{tree}", rev.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got != tree {
			t.Errorf("rev.Resolve(%s^{tree}) = %s; want %s", c1, got, tree)
		}
	}

	t.Run("none", compare)
	git(t, dir, "replace", c1, c2)
	git(t, dir, "replace", blobA, blobB)
	git(t, dir, "replace", blobB, blobC)
	t.Run("replaced", func(t *testing.T) {
		compare(t)
		objs, err := r.OpenObjects()
		if err != nil {
			t.Fatal(err)
		}
		defer objs.Close()
		if !objs.Replaced() {
			t.Error("Replaced() = false")
		}
	})
	t.Run("GIT_NO_REPLACE_OBJECTS", func(t *testing.T) {
		t.Setenv("GIT_NO_REPLACE_OBJECTS", "")
		compare(t)
	})
	t.Run("core.useReplaceRefs", func(t *testing.T) {
		git(t, dir, "config", "core.useReplaceRefs", "false")
		defer git(t, dir, "config", "--unset", "core.useReplaceRefs")
		compare(t)
	})
	t.Run("packed", func(t *testing.T) {
		git(t, dir, "pack-refs", "--all")
		compare(t)
	})
}

func TestRevSourceAndShallow(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	src := filepath.Join(base, "src")
	newRepo(t, src)
	commit(t, src, "two")
	clone := filepath.Join(base, "clone")
	git(t, base, "clone", "-q", "--depth=1", "file://"+filepath.ToSlash(src), clone)
	r, err := Discover(clone)
	if err != nil {
		t.Fatal(err)
	}
	shallow, err := r.Shallow()
	if err != nil {
		t.Fatal(err)
	}
	head := git(t, clone, "rev-parse", "HEAD")
	if len(shallow) != 1 || !shallow[mustID(t, head)] {
		t.Fatalf("Shallow = %v; want {%s}", shallow, head)
	}
	objs, err := r.OpenObjects()
	if err != nil {
		t.Fatal(err)
	}
	defer objs.Close()
	s, err := r.RevSource(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"HEAD", "HEAD^{tree}", "@{0}", "origin/HEAD"} {
		got, err := rev.Resolve(&s, spec, rev.Options{})
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if want := git(t, clone, "rev-parse", "--verify", spec); got.String() != want {
			t.Errorf("%s = %s; git %s", spec, got, want)
		}
	}
	if s.Index == nil {
		t.Fatal("no index for a work tree")
	}

	plain, err := Discover(src)
	if err != nil {
		t.Fatal(err)
	}
	if sh, err := plain.Shallow(); err != nil || len(sh) != 0 {
		t.Fatalf("Shallow without file = %v, %v", sh, err)
	}
}

func mustID(t *testing.T, s string) object.ID {
	t.Helper()
	id, err := object.ParseID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLogPolicy(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	work := filepath.Join(base, "w")
	newRepo(t, work)
	bare := filepath.Join(base, "b.git")
	git(t, base, "init", "-q", "--bare", bare)
	wr, err := Discover(work)
	if err != nil {
		t.Fatal(err)
	}
	br, err := Open(bare)
	if err != nil {
		t.Fatal(err)
	}
	check := func(r *Repository, want refs.LogPolicy) {
		t.Helper()
		got, err := r.LogPolicy()
		if err != nil || got != want {
			t.Errorf("%s: LogPolicy = %v, %v; want %v", r.GitDir, got, err, want)
		}
	}
	// git init writes logallrefupdates = true for non-bare repositories.
	check(wr, refs.LogDefault)
	git(t, work, "config", "--unset", "core.logAllRefUpdates")
	check(wr, refs.LogDefault)
	check(br, refs.LogExisting)
	git(t, bare, "config", "core.logAllRefUpdates", "always")
	check(br, refs.LogAlways)
	git(t, work, "config", "core.logAllRefUpdates", "false")
	check(wr, refs.LogExisting)
	git(t, work, "config", "core.logAllRefUpdates", "maybe")
	if _, err := wr.LogPolicy(); err == nil {
		t.Error("invalid core.logAllRefUpdates accepted")
	}
}

func TestConfigOnBranch(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "r")
	newRepo(t, dir)
	inc := filepath.Join(t.TempDir(), "inc")
	writeFile(t, inc, "[x]\n\ty = 1\n")
	branch := git(t, dir, "symbolic-ref", "--short", "HEAD")
	git(t, dir, "config", "includeIf.onbranch:"+branch+".path", inc)
	r, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := cfg.Get("x.y"); v != "1" {
		t.Fatalf("onbranch include not applied: x.y = %q", v)
	}
	git(t, dir, "checkout", "-q", "--detach")
	if cfg, err = r.Config(); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Get("x.y"); ok {
		t.Fatal("onbranch include applied with detached HEAD")
	}
}
