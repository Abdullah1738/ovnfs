package fetch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// gitTest isolates git and this package from the user's configuration.
type gitTest struct {
	t    *testing.T
	home string
	vars map[string]string // the environment Fetch sees through Options.Env
}

func newGitTest(t *testing.T) *gitTest {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	g := &gitTest{t: t, home: home, vars: map[string]string{
		"PATH":                os.Getenv("PATH"),
		"HOME":                home,
		"XDG_CONFIG_HOME":     home,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"LC_ALL":              "C",
	}}
	return g
}

// lookup is the Options.Env function: only the test's variables exist.
func (g *gitTest) lookup(key string) (string, bool) {
	v, ok := g.vars[key]
	return v, ok
}

func (g *gitTest) environ() []string {
	env := []string{
		"GIT_AUTHOR_NAME=A U Thor", "GIT_AUTHOR_EMAIL=author@example.com",
		"GIT_COMMITTER_NAME=C O Mitter", "GIT_COMMITTER_EMAIL=committer@example.com",
	}
	for k, v := range g.vars {
		env = append(env, k+"="+v)
	}
	return env
}

func (g *gitTest) git(dir string, args ...string) string {
	g.t.Helper()
	out, err := g.gitErr(dir, args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func (g *gitTest) gitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = g.environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// repo creates a repository with n commits of a few files each and
// returns its directory and the commits, oldest first.
func (g *gitTest) repo(n int, initArgs ...string) (string, []object.ID) {
	g.t.Helper()
	dir := filepath.Join(g.t.TempDir(), "src")
	g.git("", append([]string{"init", "-q", "-b", "main"}, append(initArgs, dir)...)...)
	var ids []object.ID
	for i := range n {
		for j := range 3 {
			p := filepath.Join(dir, fmt.Sprintf("d%d", j), "file.txt")
			os.MkdirAll(filepath.Dir(p), 0o777)
			os.WriteFile(p, fmt.Appendf(nil, "commit %d file %d\n", i, j), 0o666)
		}
		g.git(dir, "add", "-A")
		g.git(dir, "-c", "commit.gpgsign=false", "commit", "-q", "--date", fmt.Sprintf("%d +0000", 1700000000+i), "-m", fmt.Sprintf("c%d", i))
		ids = append(ids, g.parseID(g.git(dir, "rev-parse", "HEAD")))
	}
	return dir, ids
}

func (g *gitTest) parseID(s string) object.ID {
	g.t.Helper()
	id, err := object.ParseID(strings.TrimSpace(s))
	if err != nil {
		g.t.Fatal(err)
	}
	return id
}

// target creates an empty repository to fetch into.
func (g *gitTest) target(initArgs ...string) (dir, gitDir string, db *object.DB) {
	g.t.Helper()
	dir = filepath.Join(g.t.TempDir(), "dst")
	g.git("", append([]string{"init", "-q"}, append(initArgs, dir)...)...)
	gitDir = filepath.Join(dir, ".git")
	return dir, gitDir, g.openDB(gitDir)
}

func (g *gitTest) openDB(gitDir string) *object.DB {
	g.t.Helper()
	f := object.SHA1
	if strings.TrimSpace(g.git(gitDir, "rev-parse", "--show-object-format")) == "sha256" {
		f = object.SHA256
	}
	db, err := object.Open(filepath.Join(gitDir, "objects"), f)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { db.Close() })
	return db
}

// openLocal is an Options.OpenLocal for a work tree or bare repository.
func (g *gitTest) openLocal(path string) (*object.DB, []object.ID, error) {
	gitDir := path
	if fi, err := os.Stat(filepath.Join(path, ".git")); err == nil && fi.IsDir() {
		gitDir = filepath.Join(path, ".git")
	}
	f := object.SHA1
	if out, err := g.gitErr(gitDir, "rev-parse", "--show-object-format"); err == nil && strings.TrimSpace(out) == "sha256" {
		f = object.SHA256
	}
	db, err := object.Open(filepath.Join(gitDir, "objects"), f)
	if err != nil {
		return nil, nil, err
	}
	shallow, err := ReadShallow(gitDir)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, shallow, nil
}

// verify checks a fetch result the way the acceptance criteria ask: the
// shallow file applied, fsck --full clean, and each want's tree resolvable.
func (g *gitTest) verify(dir, gitDir string, res Result, wants ...object.ID) {
	g.t.Helper()
	if err := UpdateShallow(gitDir, res.Shallow, res.Unshallow); err != nil {
		g.t.Fatal(err)
	}
	g.git(dir, "fsck", "--full", "--no-dangling")
	for _, w := range wants {
		g.git(dir, "rev-parse", "--verify", w.String()+"^{tree}")
	}
}

func (g *gitTest) config(text string) *config.Config {
	g.t.Helper()
	c, err := config.Parse([]byte(text), "test")
	if err != nil {
		g.t.Fatal(err)
	}
	return c
}

// options returns Options fetching wants from url into the target.
func (g *gitTest) options(url string, gitDir string, db *object.DB, wants ...object.ID) Options {
	return Options{
		URL:       url,
		Wants:     wants,
		Allowed:   []string{"https", "http", "ssh", "git", "file"},
		Env:       g.lookup,
		Target:    db,
		GitDir:    gitDir,
		OpenLocal: g.openLocal,
	}
}

func idStrings(ids []object.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
