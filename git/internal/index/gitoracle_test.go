package index

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// testRepo is a scratch repository driven by the git binary, the oracle of
// the differential tests.
type testRepo struct {
	t      *testing.T
	dir    string
	format object.Format
	env    []string
}

func newTestRepo(t *testing.T, f object.Format) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	r := &testRepo{t: t, dir: t.TempDir(), format: f}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			r.env = append(r.env, kv)
		}
	}
	r.env = append(r.env,
		"HOME="+home, "XDG_CONFIG_HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "LC_ALL=C",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com", "GIT_COMMITTER_DATE=1700000000 +0000",
	)
	r.git("init", "-q", "--object-format="+f.String(), ".")
	r.git("config", "core.hooksPath", os.DevNull)
	r.git("config", "advice.sparseIndexExpanded", "false")
	return r
}

// run runs git with extra environment and stdin, returning stdout.
func (r *testRepo) run(env []string, stdin string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(append([]string(nil), r.env...), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	return r.run(nil, "", args...)
}

// withIndex runs git against the index file name.
func (r *testRepo) withIndex(name string, args ...string) string {
	r.t.Helper()
	return r.run([]string{"GIT_INDEX_FILE=" + name}, "", args...)
}

func (r *testRepo) path(rel string) string { return filepath.Join(r.dir, filepath.FromSlash(rel)) }

func (r *testRepo) writeFile(rel, content string) {
	r.t.Helper()
	p := r.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o666); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) blob(content string) object.ID {
	r.t.Helper()
	return r.id(r.run(nil, content, "hash-object", "-w", "--stdin"))
}

func (r *testRepo) id(hex string) object.ID {
	r.t.Helper()
	id, err := object.ParseIDFormat(r.format, strings.TrimSpace(hex))
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// readTree is a TreeReader backed by `git cat-file tree`.
func (r *testRepo) readTree(id object.ID) ([]object.TreeEntry, error) {
	cmd := exec.Command("git", "cat-file", "tree", id.String())
	cmd.Dir = r.dir
	cmd.Env = r.env
	data, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return object.ParseTree(r.format, data)
}

// lsStage formats entries as `git ls-files --stage -z` does.
func lsStage(entries []Entry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s %d\t%s\x00", e.Mode, e.ID, e.Stage, e.Path)
	}
	return b.String()
}

// lsDebug formats entries' stat data as `git ls-files --debug` does, minus
// the in-memory flags word.
func lsDebug(entries []Entry) string {
	var b strings.Builder
	for _, e := range entries {
		s := e.Stat
		fmt.Fprintf(&b, "%s\n  ctime: %d:%d\n  mtime: %d:%d\n  dev: %d\tino: %d\n  uid: %d\tgid: %d\n  size: %d\n",
			e.Path, s.CTime.Sec, s.CTime.Nsec, s.MTime.Sec, s.MTime.Nsec, s.Dev, s.Ino, s.UID, s.GID, s.Size)
	}
	return b.String()
}

// gitDebug returns `git ls-files --debug` for index name, flags words removed.
func (r *testRepo) gitDebug(name string, args ...string) string {
	r.t.Helper()
	out := r.withIndex(name, append([]string{"ls-files", "--debug"}, args...)...)
	var b strings.Builder
	for line := range strings.Lines(out) {
		if i := strings.Index(line, "\tflags: "); i >= 0 {
			line = line[:i] + "\n"
		}
		b.WriteString(line)
	}
	return b.String()
}

func readFileBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	info, err := os.Stat(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, readFileBytes(t, from), 0o666); err != nil {
		t.Fatal(err)
	}
	// Keep the mtime: it is the racy-git reference of both git and Read.
	if err := os.Chtimes(to, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, name string, f object.Format, opts ReadOptions) *Index {
	t.Helper()
	idx, err := Read(name, f, opts)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

var formats = []object.Format{object.SHA1, object.SHA256}

func forEachFormat(t *testing.T, test func(t *testing.T, f object.Format)) {
	for _, f := range formats {
		t.Run(f.String(), func(t *testing.T) { test(t, f) })
	}
}
