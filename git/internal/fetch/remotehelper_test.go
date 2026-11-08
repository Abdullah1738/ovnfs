package fetch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitHelpers lets findProgram reach git's own remote helpers wherever the
// installation keeps them.
func gitHelpers(g *gitTest) {
	g.vars["GIT_EXEC_PATH"] = g.git("", "--exec-path")
}

// connect: git-remote-ext runs upload-pack and relays its stream.
func TestFetchRemoteHelperConnect(t *testing.T) {
	s := newServer(t)
	gitHelpers(s.g)
	s.fetchScenario(t, "ext::git %s "+s.dir, func(o *Options) { o.Allowed = append(o.Allowed, "ext") })
}

// stateless-connect: git-remote-http relays protocol v2 requests to a
// smart HTTP server.
func TestFetchRemoteHelperStatelessConnect(t *testing.T) {
	s := newServer(t)
	gitHelpers(s.g)
	hs := s.startHTTP(t, httpConfig{})
	s.fetchScenario(t, "http::"+hs.URL+"/src", nil)
	for _, r := range hs.log() {
		if !strings.HasSuffix(r, " version=2") {
			t.Fatalf("helper request without v2: %s", r)
		}
	}
}

// fetch: the helper stores the objects itself, given wants named after the
// listed refs pointing at them, else by id.
func TestFetchRemoteHelperFetch(t *testing.T) {
	needScripts(t)
	s := newServer(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	script := fmt.Sprintf(`#!/bin/sh
echo "args=$* GIT_DIR=$GIT_DIR" >> %[1]q
while IFS= read -r line; do
	echo "$line" >> %[1]q
	case "$line" in
	capabilities) printf 'fetch\noption\nobject-format\n\n' ;;
	option*) echo ok ;;
	list) printf ':object-format sha1\n%[2]s refs/heads/main\n@refs/heads/main HEAD\n\n' ;;
	fetch*) fetching=1 ;;
	"")
		[ -n "$fetching" ] || exit 0
		cp -R %[3]q/. "$GIT_DIR/objects/"
		: > "$GIT_DIR/objects/pack/fake.keep"
		printf 'lock %%s\n\n' "$GIT_DIR/objects/pack/fake.keep"
		fetching=
		;;
	esac
done
`, logPath, s.ids[4], filepath.Join(s.dir, ".git", "objects"))
	if err := os.WriteFile(filepath.Join(dir, "git-remote-fake"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s.g.vars["PATH"] = dir + string(os.PathListSeparator) + s.g.vars["PATH"]

	target, gitDir, db := s.g.target()
	opts := s.g.options("fake::somewhere", gitDir, db, s.ids[4], s.ids[2])
	opts.Allowed = append(opts.Allowed, "fake")
	opts.Depth = 1
	res, err := Fetch(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	s.g.verify(target, gitDir, res, s.ids[4], s.ids[2])
	if _, err := os.Stat(filepath.Join(gitDir, "objects", "pack", "fake.keep")); !os.IsNotExist(err) {
		t.Fatalf("lock file left: %v", err)
	}
	abs, _ := filepath.Abs(gitDir)
	want := fmt.Sprintf(`args=fake::somewhere somewhere GIT_DIR=%s
capabilities
option progress false
option verbosity 0
option object-format true
list
fetch %s refs/heads/main
fetch %s %s


`, abs, s.ids[4], s.ids[2], s.ids[2])
	if log, _ := os.ReadFile(logPath); string(log) != want {
		t.Fatalf("helper conversation:\n%s\nwant:\n%s", log, want)
	}
}
