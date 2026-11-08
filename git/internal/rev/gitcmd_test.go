package rev

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRunner runs the real git as the differential-test oracle, isolated
// from the user's configuration.
type gitRunner struct {
	t   *testing.T
	env []string
}

func newGit(t *testing.T) *gitRunner {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"LC_ALL=C",
		"GIT_AUTHOR_NAME=Author",
		"GIT_AUTHOR_EMAIL=author@example.com",
		"GIT_COMMITTER_NAME=Committer",
		"GIT_COMMITTER_EMAIL=committer@example.com",
		"PATH=" + os.Getenv("PATH"),
	}
	if root := os.Getenv("SYSTEMROOT"); root != "" {
		env = append(env, "SYSTEMROOT="+root)
	}
	return &gitRunner{t: t, env: env}
}

// run runs git in dir and returns its trimmed stdout, failing the test on error.
func (g *gitRunner) run(dir string, args ...string) string {
	g.t.Helper()
	out, err := g.try(dir, nil, args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// runEnv is run with extra environment variables.
func (g *gitRunner) runEnv(dir string, extra []string, args ...string) string {
	g.t.Helper()
	out, err := g.try(dir, extra, args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// runInput is run with stdin.
func (g *gitRunner) runInput(dir, stdin string, args ...string) string {
	g.t.Helper()
	out, err := g.exec(dir, nil, stdin, args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// try runs git and returns its trimmed stdout (stderr too on failure).
func (g *gitRunner) try(dir string, extra []string, args ...string) (string, error) {
	return g.exec(dir, extra, "", args...)
}

func (g *gitRunner) exec(dir string, extra []string, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(append([]string{}, g.env...), extra...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String() + stderr.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}
