package object

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// testRepo is a scratch repository driven by the real git binary, the oracle
// for differential tests.
type testRepo struct {
	t      *testing.T
	dir    string // work tree, or the repository itself when bare
	gitDir string
	env    []string
	format Format
}

// newRepo runs git init with args in a fresh directory, skipping the test
// when git is not installed.
func newRepo(t *testing.T, args ...string) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o755)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"LC_ALL=C",
		"GIT_AUTHOR_NAME=A U Thor",
		"GIT_AUTHOR_EMAIL=author@example.com",
		"GIT_AUTHOR_DATE=1700000000 +0100",
		"GIT_COMMITTER_NAME=C O Mitter",
		"GIT_COMMITTER_EMAIL=committer@example.com",
		"GIT_COMMITTER_DATE=1700000000 +0100",
	)
	r := &testRepo{t: t, dir: filepath.Join(root, "repo"), env: env, format: SHA1}
	r.git(nil, append(append([]string{"init", "-q"}, args...), r.dir)...)
	r.gitDir = filepath.Join(r.dir, ".git")
	if _, err := os.Stat(r.gitDir); err != nil {
		r.gitDir = r.dir
	}
	r.git(nil, "config", "gc.auto", "0")
	f, err := ParseFormat(strings.TrimSpace(r.gitString("rev-parse", "--show-object-format")))
	if err != nil {
		t.Fatal(err)
	}
	r.format = f
	return r
}

func (r *testRepo) objects() string { return filepath.Join(r.gitDir, "objects") }

func (r *testRepo) cmd(stdin []byte, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Dir = filepath.Dir(r.dir)
	if _, err := os.Stat(r.dir); err == nil {
		cmd.Dir = r.dir
	}
	cmd.Env = r.env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd
}

// git runs a git command in the repository and returns its stdout, failing
// the test on error.
func (r *testRepo) git(stdin []byte, args ...string) []byte {
	r.t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := r.cmd(stdin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return stdout.Bytes()
}

func (r *testRepo) gitString(args ...string) string { return string(r.git(nil, args...)) }

func (r *testRepo) id(hex string) ID {
	r.t.Helper()
	id, err := ParseIDFormat(r.format, strings.TrimSpace(hex))
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *testRepo) open() *DB {
	r.t.Helper()
	db, err := Open(r.objects(), r.format)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { db.Close() })
	return db
}

type gitObject struct {
	kind Kind
	data []byte
}

// allObjects returns every object git sees in the repository, alternates
// included, via cat-file --batch-all-objects --batch.
func (r *testRepo) allObjects() map[ID]gitObject {
	r.t.Helper()
	out := bufio.NewReader(bytes.NewReader(r.git(nil, "cat-file", "--batch-all-objects", "--batch")))
	objs := map[ID]gitObject{}
	for {
		line, err := out.ReadString('\n')
		if err == io.EOF {
			return objs
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			r.t.Fatalf("cat-file line %q", line)
		}
		kind, err := ParseKind(f[1])
		if err != nil {
			r.t.Fatal(err)
		}
		size, _ := strconv.Atoi(f[2])
		data := make([]byte, size+1)
		if _, err := io.ReadFull(out, data); err != nil {
			r.t.Fatal(err)
		}
		objs[r.id(f[0])] = gitObject{kind, data[:size]}
	}
}

// fastImport feeds a fast-import stream to the repository, which stores the
// objects as a new pack.
func (r *testRepo) fastImport(stream string) {
	r.t.Helper()
	r.git([]byte(stream), "-c", "fastimport.unpackLimit=0", "fast-import", "--quiet", "--done")
}

// fastImportLoose is fastImport storing the objects loose.
func (r *testRepo) fastImportLoose(stream string) {
	r.t.Helper()
	r.git([]byte(stream), "-c", "fastimport.unpackLimit=1000000", "fast-import", "--quiet", "--done")
}

// history builds a fast-import stream of n commits on refs/heads/main that
// exercises deltas (a large file edited a little each time), executables,
// symlinks, gitlinks, directory renames, file/directory swaps and annotated
// tags (including a tag of a tag). Content is deterministic, varied by seed.
func history(f Format, n int, seed string) string {
	var s strings.Builder
	data := func(b string) { fmt.Fprintf(&s, "data %d\n%s\n", len(b), b) }
	lines := make([]string, 400)
	for i := range lines {
		lines[i] = fmt.Sprintf("%s line %d: the quick brown fox jumps over the lazy dog %d", seed, i, i*i)
	}
	for c := 1; c <= n; c++ {
		for k := range 3 {
			i := (c*37 + k*101) % len(lines)
			lines[i] = fmt.Sprintf("%s commit %d edit %d", seed, c, k)
		}
		if c%5 == 0 {
			lines = append(lines, fmt.Sprintf("%s appended %d", seed, c))
		}
		fmt.Fprintf(&s, "commit refs/heads/main\nmark :%d\n", c)
		fmt.Fprintf(&s, "author A U Thor <author@example.com> %d +0100\n", 1700000000+c)
		fmt.Fprintf(&s, "committer C O Mitter <committer@example.com> %d -0230\n", 1700000000+c)
		data(fmt.Sprintf("%s commit %d\n\nbody", seed, c))
		if c > 1 {
			fmt.Fprintf(&s, "from :%d\n", c-1)
		}
		s.WriteString("M 100644 inline big.txt\n")
		data(strings.Join(lines, "\n"))
		fmt.Fprintf(&s, "M 100644 inline dir%d/sub/file.go\n", c%3)
		data(fmt.Sprintf("package sub // %s %d\n%s", seed, c, strings.Join(lines[:50+c%40], "\n")))
		if c%4 == 0 {
			fmt.Fprintf(&s, "D dir%d\n", (c+1)%3)
		}
		mode := "100644"
		if c%2 == 0 {
			mode = "100755"
		}
		fmt.Fprintf(&s, "M %s inline bin/tool\n", mode)
		data("#!/bin/sh\necho " + seed)
		fmt.Fprintf(&s, "M 120000 inline link\n")
		data(fmt.Sprintf("big.txt%d", c%2))
		if c%3 == 0 {
			// swap: x is a file, then a directory, then a file again
			s.WriteString("D x\nM 100644 inline x/inner\n")
			data(fmt.Sprintf("%d", c))
		} else {
			s.WriteString("D x\nM 100644 inline x\n")
			data(fmt.Sprintf("x %d", c))
		}
		if c%7 == 0 {
			fmt.Fprintf(&s, "M 160000 %s sub/module\n", strings.Repeat(fmt.Sprintf("%x", c%16), f.HexSize()))
		}
		s.WriteString("\n")
		if c%10 == 0 {
			fmt.Fprintf(&s, "tag v%d\nfrom :%d\ntagger T Agger <tagger@example.com> %d +0000\n", c, c, 1700000000+c)
			data(fmt.Sprintf("release %d", c))
		}
	}
	s.WriteString("done\n")
	return s.String()
}
