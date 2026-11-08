package refs

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// gitRepo is a scratch repository driven by the real git binary, the oracle
// for differential tests.
type gitRepo struct {
	t       *testing.T
	dir     string // work tree
	gitDir  string
	format  object.Format
	backend Backend
	env     []string
}

// requireGit skips the test when git is not installed.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// isolatedGitEnv is the environment with every GIT_* variable dropped and
// user and system configuration disabled, so the oracle sees only the
// test's repository.
func isolatedGitEnv(t *testing.T) []string {
	home := t.TempDir()
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			env = append(env, kv)
		}
	}
	return append(env,
		"HOME="+home, "XDG_CONFIG_HOME="+home, "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=A U Thor", "GIT_AUTHOR_EMAIL=author@example.com",
		"GIT_COMMITTER_NAME=C O Mitter", "GIT_COMMITTER_EMAIL=committer@example.com",
		"GIT_AUTHOR_DATE=1700000000 +0130", "GIT_COMMITTER_DATE=1700000000 +0130",
	)
}

// newGitRepo runs git init with the given ref and object formats.
func newGitRepo(t *testing.T, backend Backend, format object.Format) *gitRepo {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	r := &gitRepo{t: t, dir: dir, gitDir: filepath.Join(dir, ".git"), format: format, backend: backend, env: isolatedGitEnv(t)}
	r.git("init", "-q", "--initial-branch=main", "--ref-format="+string(backend), "--object-format="+format.String())
	r.git("config", "core.hooksPath", os.DevNull)
	return r
}

// git runs git in the work tree and returns its trimmed stdout, failing the
// test on a non-zero exit.
func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	out, err := r.tryGit(args...)
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// tryGit runs git and returns its trimmed stdout, or stderr with the error.
func (r *gitRepo) tryGit(args ...string) (string, error) {
	stdout, stderr, err := runGitIn(r.dir, r.env, args...)
	if err != nil {
		return stderr, err
	}
	return stdout, nil
}

// gitStdin runs git with input on stdin, failing the test on error.
func (r *gitRepo) gitStdin(input string, args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env, cmd.Stdin = r.dir, r.env, strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// runGitIn runs git in dir and returns its trimmed stdout and stderr.
func runGitIn(dir string, env []string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return strings.TrimSpace(out.String()), strings.TrimSpace(errOut.String()), err
}

// store opens the repository's main worktree refs.
func (r *gitRepo) store() *Store {
	r.t.Helper()
	s, err := Open(Options{GitDir: r.gitDir, Format: r.format, Backend: r.backend})
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// id parses a hex object name printed by git.
func (r *gitRepo) id(hex string) object.ID {
	r.t.Helper()
	id, err := object.ParseIDFormat(r.format, hex)
	if err != nil {
		r.t.Fatalf("parse %q: %v", hex, err)
	}
	return id
}

// commit makes an empty commit on HEAD and returns its name.
func (r *gitRepo) commit(msg string) object.ID {
	r.t.Helper()
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.id(r.git("rev-parse", "HEAD"))
}

// matrix runs f for every backend and object format.
func matrix(t *testing.T, f func(t *testing.T, backend Backend, format object.Format)) {
	for _, backend := range []Backend{BackendFiles, BackendReftable} {
		for _, format := range []object.Format{object.SHA1, object.SHA256} {
			t.Run(string(backend)+"/"+format.String(), func(t *testing.T) { f(t, backend, format) })
		}
	}
}
