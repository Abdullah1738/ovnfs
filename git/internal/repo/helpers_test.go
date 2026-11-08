package repo

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolate gives the test (and the git processes it runs, which inherit the
// environment) a private HOME and no system or global configuration, and
// clears every variable that would change what git or this package does.
func isolate(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	for _, k := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
		"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
		"GIT_CONFIG_SYSTEM", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE", "EMAIL", "GIT_DEFAULT_HASH",
		"GIT_DEFAULT_REF_FORMAT", "GIT_DEFAULT_REF_STORAGE_FORMAT", "GIT_TEMPLATE_DIR", "SUDO_UID",
	} {
		unsetenv(t, k)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("LC_ALL", "C")
}

// unsetenv removes k for the rest of the test, restoring it afterwards.
func unsetenv(t *testing.T, k string) {
	t.Helper()
	t.Setenv(k, "")
	os.Unsetenv(k)
}

// gitResult runs git in dir and returns its trimmed stdout and stderr.
func gitResult(dir string, args ...string) (string, string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

// git runs git in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, stderr, err := gitResult(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return out
}

// gitInput runs git in dir with stdin.
func gitInput(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// newRepo creates a non-bare repository at dir with one commit.
func newRepo(t *testing.T, dir string, initArgs ...string) {
	t.Helper()
	git(t, filepath.Dir(dir), append(append([]string{"init", "-q"}, initArgs...), dir)...)
	commit(t, dir, "init")
}

// commit records an empty commit with a fixed identity.
func commit(t *testing.T, dir, msg string) {
	t.Helper()
	git(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", msg)
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// checkDiscover compares Discover(dir) with what
// `git rev-parse --is-inside-work-tree --show-toplevel --git-common-dir --git-dir`
// says from dir, returning the repository when both found one. Git for
// Windows prints paths with forward slashes.
func checkDiscover(t *testing.T, dir string) *Repository {
	t.Helper()
	out, stderr, gitErr := gitResult(dir, "rev-parse", "--path-format=absolute",
		"--is-inside-work-tree", "--show-toplevel", "--git-common-dir", "--git-dir")
	r, err := Discover(dir)
	lines := strings.Split(out, "\n")
	var want error
	switch {
	case gitErr == nil && lines[0] == "true" && len(lines) == 4:
		if err != nil {
			t.Fatalf("Discover(%s): %v; git found %q", dir, err, lines[1:])
		}
		got := []string{r.Top, r.CommonDir, r.GitDir}
		for i, g := range got {
			if g != filepath.FromSlash(lines[i+1]) {
				t.Fatalf("Discover(%s) = top %q common %q gitdir %q; git: %q", dir, got[0], got[1], got[2], lines[1:])
			}
		}
		if r.Bare {
			t.Fatalf("Discover(%s) reports a bare repository", dir)
		}
		return r
	case lines[0] == "false" || strings.Contains(stderr, "must be run in a work tree"):
		want = ErrNoWorkTree
	case strings.Contains(stderr, "not a git repository"):
		want = ErrNotRepository
	case strings.Contains(stderr, "gitfile"):
		want = ErrInvalidGitfile
	case strings.Contains(stderr, "repo version"), strings.Contains(stderr, "repository extension"),
		strings.Contains(stderr, "invalid value for 'extensions"):
		want = ErrUnsupportedFormat
	default:
		t.Fatalf("unclassified git result from %s: %q %q %v", dir, out, stderr, gitErr)
	}
	if !errors.Is(err, want) {
		t.Fatalf("Discover(%s) = %+v, %v; want %v (git: %q %q)", dir, r, err, want, out, stderr)
	}
	return nil
}
