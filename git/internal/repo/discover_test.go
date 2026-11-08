package repo

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverMatchesGit(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	main := filepath.Join(base, "main")
	newRepo(t, main)
	mkdirs(t, filepath.Join(main, "a", "b", "c"))

	// A linked worktree.
	linked := filepath.Join(base, "linked")
	git(t, main, "worktree", "add", "-q", "-b", "side", linked)
	mkdirs(t, filepath.Join(linked, "d"))

	// An absorbed submodule.
	subsrc := filepath.Join(base, "subsrc")
	newRepo(t, subsrc)
	super := filepath.Join(base, "super")
	newRepo(t, super)
	git(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subsrc, "sub")
	mkdirs(t, filepath.Join(super, "sub", "x"))

	// A symlink into the work tree.
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(main, "a"), link); err != nil {
		t.Fatal(err)
	}

	// A bare repository.
	bare := filepath.Join(base, "bare.git")
	git(t, base, "init", "-q", "--bare", bare)

	// core.worktree pointing elsewhere.
	cw := filepath.Join(base, "cw")
	newRepo(t, cw)
	mkdirs(t, filepath.Join(base, "cw-tree", "y"), filepath.Join(cw, "z"))
	git(t, cw, "config", "core.worktree", "../../cw-tree")

	// A hand-written gitfile with a relative path, and a nested invalid .git.
	gf := filepath.Join(base, "gf")
	mkdirs(t, filepath.Join(gf, "w"))
	writeFile(t, filepath.Join(gf, ".git"), "gitdir: ../main/.git\r\n")
	sl := filepath.Join(base, "sl")
	mkdirs(t, filepath.Join(sl, "q"))
	if err := os.Symlink(filepath.Join(main, ".git"), filepath.Join(sl, ".git")); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, filepath.Join(main, "a", "fake", ".git"))
	writeFile(t, filepath.Join(main, "a", "fake", ".git", "HEAD"), "garbage\n")

	for _, dir := range []string{
		main,
		filepath.Join(main, "a", "b", "c"),
		filepath.Join(main, "a", "fake"),
		filepath.Join(link, "b"),
		linked,
		filepath.Join(linked, "d"),
		filepath.Join(super, "sub"),
		filepath.Join(super, "sub", "x"),
		filepath.Join(base, "cw-tree", "y"),
		filepath.Join(cw, "z"),
		cw,
		filepath.Join(gf, "w"),
		filepath.Join(sl, "q"),
		bare,
		filepath.Join(bare, "objects"),
		filepath.Join(main, ".git"),
		filepath.Join(main, ".git", "refs", "heads"),
		filepath.Join(main, ".git", "worktrees", "linked"),
	} {
		t.Run(strings.TrimPrefix(dir, base), func(t *testing.T) { checkDiscover(t, dir) })
	}

	// On a case-insensitive filesystem git reports the on-disk spelling.
	if folded := filepath.Join(base, "MAIN", "A"); fileExists(folded) {
		t.Run("case-folded", func(t *testing.T) { checkDiscover(t, folded) })
	}

	t.Run("ceiling", func(t *testing.T) {
		deep := filepath.Join(main, "a", "b", "c")
		for _, ceiling := range []string{
			main,
			filepath.Join(main, "a"),
			filepath.Join(main, "a") + string(filepath.Separator),
			filepath.Join(main, "a", "b", "c"),
			string(filepath.ListSeparator) + filepath.Join(main, "a"),
			"relative" + string(filepath.ListSeparator) + filepath.Join(main, "a"),
			filepath.Join(link),
			filepath.Join(base, "mai"),
		} {
			t.Setenv("GIT_CEILING_DIRECTORIES", ceiling)
			checkDiscover(t, deep)
			checkDiscover(t, filepath.Join(link, "b"))
		}
	})
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestDiscoverValidity(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	newRepo(t, outer)
	inner := filepath.Join(outer, "in")
	mkdirs(t, filepath.Join(inner, "x"))
	head := filepath.Join(outer, ".git", "HEAD")
	saved, err := os.ReadFile(head)
	if err != nil {
		t.Fatal(err)
	}
	restore := func() {
		os.Remove(head)
		writeFile(t, head, string(saved))
	}
	heads := map[string]string{
		"garbage":    "garbage\n",
		"ref-foo":    "ref: foo\n",
		"ref-nosp":   "ref:refs/heads/x\n",
		"ref-dotdot": "ref: refs/heads/../x\n",
		"ref-refs":   "ref: refs\n",
		"lead-space": "  ref: refs/heads/x\n",
		"sha1":       strings.Repeat("0", 40) + "\n",
		"sha1-junk":  strings.Repeat("a", 40) + "junk",
		"short":      strings.Repeat("0", 39) + "\n",
		"nonhex":     strings.Repeat("z", 40) + "\n",
	}
	for name, content := range heads {
		t.Run("HEAD-"+name, func(t *testing.T) {
			defer restore()
			writeFile(t, head, content)
			checkDiscover(t, filepath.Join(inner, "x"))
		})
	}
	for name, target := range map[string]string{"sym-refs": "refs/heads/missing", "sym-dot": "./refs/heads/main", "sym-abs": filepath.Join(outer, ".git", "refs", "heads", "main")} {
		t.Run("HEAD-"+name, func(t *testing.T) {
			defer restore()
			os.Remove(head)
			if err := os.Symlink(target, head); err != nil {
				t.Fatal(err)
			}
			checkDiscover(t, filepath.Join(inner, "x"))
		})
	}
	for _, sub := range []string{"objects", "refs"} {
		t.Run("no-"+sub, func(t *testing.T) {
			p := filepath.Join(outer, ".git", sub)
			if err := os.Rename(p, p+".off"); err != nil {
				t.Fatal(err)
			}
			defer os.Rename(p+".off", p)
			checkDiscover(t, filepath.Join(inner, "x"))
		})
	}
	gitfiles := map[string]string{
		"garbage":       "garbage\n",
		"empty":         "",
		"no-space":      "gitdir:../.git\n",
		"no-path":       "gitdir: \n",
		"missing":       "gitdir: /nonexistent/repo\n",
		"trailing-sp":   "gitdir: ../.git \n",
		"extra-line":    "gitdir: ../.git\nextra\n",
		"relative":      "gitdir: ../.git\n\n\n",
		"to-non-repo":   "gitdir: x\n",
		"crlf-relative": "gitdir: ../.git\r\n",
	}
	for name, content := range gitfiles {
		t.Run("gitfile-"+name, func(t *testing.T) {
			p := filepath.Join(inner, ".git")
			writeFile(t, p, content)
			defer os.Remove(p)
			checkDiscover(t, filepath.Join(inner, "x"))
		})
	}
}

func TestDiscoverWorktreeLayouts(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	main := filepath.Join(base, "m")
	newRepo(t, main)
	wt := filepath.Join(base, "wt")
	git(t, main, "worktree", "add", "-q", "-b", "w", wt)
	mkdirs(t, filepath.Join(wt, "d"), filepath.Join(base, "elsewhere"))
	admin := filepath.Join(main, ".git", "worktrees", "wt")
	commondir := filepath.Join(admin, "commondir")
	cfg := filepath.Join(main, ".git", "config")
	start := filepath.Join(wt, "d")

	for name, content := range map[string]string{
		"commondir-abs":     filepath.Join(main, ".git") + "\n",
		"commondir-crlf":    "../..\r\n",
		"commondir-nonl":    "../..",
		"commondir-missing": "../nonexistent\n",
		"commondir-space":   "../.. \n",
	} {
		t.Run(name, func(t *testing.T) {
			writeFile(t, commondir, content)
			defer writeFile(t, commondir, "../..\n")
			checkDiscover(t, start)
		})
	}
	t.Run("common-core-ignored", func(t *testing.T) {
		git(t, main, "config", "-f", cfg, "core.bare", "true")
		git(t, main, "config", "-f", cfg, "core.worktree", filepath.Join(base, "elsewhere"))
		defer git(t, base, "config", "-f", cfg, "--unset", "core.worktree")
		defer git(t, base, "config", "-f", cfg, "core.bare", "false")
		checkDiscover(t, start)
	})
	t.Run("worktree-config", func(t *testing.T) {
		git(t, base, "config", "-f", cfg, "core.repositoryformatversion", "1")
		git(t, base, "config", "-f", cfg, "extensions.worktreeConfig", "true")
		defer git(t, base, "config", "-f", cfg, "--unset", "extensions.worktreeConfig")
		writeFile(t, filepath.Join(admin, "config.worktree"), "[core]\n\tbare = true\n")
		checkDiscover(t, start)
		writeFile(t, filepath.Join(admin, "config.worktree"), "[core]\n\tworktree = "+filepath.ToSlash(filepath.Join(base, "elsewhere"))+"\n")
		checkDiscover(t, start)
		checkDiscover(t, filepath.Join(base, "elsewhere"))
		os.Remove(filepath.Join(admin, "config.worktree"))
		git(t, base, "config", "-f", cfg, "core.bare", "true")
		checkDiscover(t, start)
		git(t, base, "config", "-f", cfg, "core.bare", "false")
		checkDiscover(t, start)
		checkDiscover(t, main)
	})
}

func TestRepositoryFormat(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "r")
	newRepo(t, dir)
	mkdirs(t, filepath.Join(dir, "sub"))
	cfg := filepath.Join(dir, ".git", "config")
	set := func(t *testing.T, args ...string) {
		t.Helper()
		git(t, filepath.Dir(dir), append([]string{"config", "-f", cfg}, args...)...)
	}
	exts := []struct{ name, value string }{
		{"noop", "true"}, {"noop-v1", "true"}, {"foo", "bar"}, {"preciousObjects", "true"},
		{"partialClone", "origin"}, {"worktreeConfig", "true"}, {"relativeWorktrees", "true"},
		{"submodulePathConfig", "true"}, {"objectFormat", "sha1"}, {"objectFormat", "md5"},
		{"objectFormat", "SHA1"}, {"refStorage", "files"}, {"refStorage", "foo"},
	}
	for _, version := range []string{"0", "1"} {
		for _, e := range exts {
			t.Run(version+"/"+e.name+"="+e.value, func(t *testing.T) {
				set(t, "core.repositoryformatversion", version)
				set(t, "extensions."+e.name, e.value)
				defer set(t, "--unset", "extensions."+e.name)
				checkDiscover(t, filepath.Join(dir, "sub"))
			})
		}
		// git accepts compatObjectFormat only when built with Rust, so it is
		// no oracle here; Discover always refuses it (see v1Extensions).
		t.Run(version+"/compatObjectFormat=sha256", func(t *testing.T) {
			set(t, "core.repositoryformatversion", version)
			set(t, "extensions.compatObjectFormat", "sha256")
			defer set(t, "--unset", "extensions.compatObjectFormat")
			if _, err := Discover(filepath.Join(dir, "sub")); !errors.Is(err, ErrUnsupportedFormat) {
				t.Fatalf("Discover: %v; want ErrUnsupportedFormat", err)
			}
		})
	}
	for _, version := range []string{"2", "-1", "abc"} {
		t.Run("version="+version, func(t *testing.T) {
			set(t, "core.repositoryformatversion", version)
			defer set(t, "core.repositoryformatversion", "0")
			_, err := Discover(filepath.Join(dir, "sub"))
			_, _, gitErr := gitResult(filepath.Join(dir, "sub"), "rev-parse", "--git-dir")
			if (err == nil) != (gitErr == nil) {
				t.Fatalf("Discover: %v; git: %v", err, gitErr)
			}
			if version == "2" && !errors.Is(err, ErrUnsupportedFormat) {
				t.Fatalf("Discover: %v; want ErrUnsupportedFormat", err)
			}
		})
	}
	t.Run("includes-ignored", func(t *testing.T) {
		inc := filepath.Join(t.TempDir(), "inc")
		writeFile(t, inc, "[core]\n\trepositoryformatversion = 2\n\tbare = true\n")
		set(t, "include.path", inc)
		defer set(t, "--unset", "include.path")
		checkDiscover(t, filepath.Join(dir, "sub"))
	})
}

func TestSHA256AndReftable(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "r")
	newRepo(t, dir, "--object-format=sha256", "--ref-format=reftable")
	r := checkDiscover(t, dir)
	if r.Format.String() != "sha256" || r.RefBackend != "reftable" {
		t.Fatalf("Format %v RefBackend %q", r.Format, r.RefBackend)
	}
	head, err := r.Refs().Resolve("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if want := git(t, dir, "rev-parse", "HEAD"); head.ID.String() != want {
		t.Fatalf("HEAD = %s, git %s", head.ID, want)
	}
	objs, err := r.OpenObjects()
	if err != nil {
		t.Fatal(err)
	}
	defer objs.Close()
	if !objs.Has(head.ID) {
		t.Fatal("HEAD commit not found")
	}
}

func TestDiscoverUnsafe(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "r")
	newRepo(t, dir)
	top := git(t, dir, "rev-parse", "--path-format=absolute", "--show-toplevel")
	defer func(f func(string) (bool, error)) { isOwned = f }(isOwned)
	isOwned = func(string) (bool, error) { return false, nil }

	global := filepath.Join(t.TempDir(), "global")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for _, tc := range []struct {
		config string
		safe   bool
	}{
		{"", false},
		{"[safe]\n\tdirectory = " + top + "\n", true},
		{"[safe]\n\tdirectory = *\n", true},
		{"[safe]\n\tdirectory = *\n\tdirectory =\n", false},
		{"[safe]\n\tdirectory = " + path.Dir(top) + "/*\n", true},
		{"[safe]\n\tdirectory = " + top + "/*\n", false},
		{"[safe]\n\tdirectory = " + top + "x\n", false},
	} {
		writeFile(t, global, tc.config)
		r, err := Discover(dir)
		if tc.safe && err != nil || !tc.safe && !errors.Is(err, ErrUnsafe) {
			t.Errorf("config %q: Discover = %+v, %v", tc.config, r, err)
		}
	}
	// safe.directory in the repository's own config does not count.
	writeFile(t, global, "")
	git(t, dir, "config", "safe.directory", "*")
	if _, err := Discover(dir); !errors.Is(err, ErrUnsafe) {
		t.Errorf("local safe.directory honoured: %v", err)
	}
}

func TestSafeDirectoryValues(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, tc := range []struct {
		values []string
		dir    string
		safe   bool
	}{
		{nil, "/r", false},
		{[]string{"/r"}, "/r", true},
		{[]string{"/r/"}, "/r", true},
		{[]string{"/r", ""}, "/r", false},
		{[]string{"", "/r"}, "/r", true},
		{[]string{"/a/*"}, "/a/b/c", true},
		{[]string{"/a/*"}, "/a", false},
		{[]string{"/ab/*"}, "/a/b", false},
		{[]string{"~/r"}, "/home/u/r", true},
		{[]string{"%(prefix)/r"}, "/r", false},
		{[]string{"*", "/x"}, "/r", true},
	} {
		if got := safeDirectory(tc.values, filepath.FromSlash(tc.dir)); got != tc.safe {
			t.Errorf("safeDirectory(%q, %s) = %v", tc.values, tc.dir, got)
		}
	}
}
