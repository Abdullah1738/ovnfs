package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// startDumb serves the directory holding s's work tree as plain files, its
// repository at /src/.git with the first three commits packed and the last
// two loose, as a dumb HTTP server publishes them.
func (s *server) startDumb(t *testing.T) *httpServer {
	gitDir := filepath.Join(s.dir, ".git")
	cmd := exec.Command("git", "pack-objects", "--revs", "-q", filepath.Join(gitDir, "objects", "pack", "pack"))
	cmd.Dir, cmd.Env = s.dir, s.g.environ()
	cmd.Stdin = strings.NewReader(s.ids[2].String() + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pack-objects: %v\n%s", err, out)
	}
	s.g.git(s.dir, "prune-packed")
	s.g.git(s.dir, "update-server-info")
	files := http.FileServer(http.Dir(filepath.Dir(s.dir)))
	hs := &httpServer{}
	hs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hs.mu.Lock()
		hs.requests = append(hs.requests, r.Method+" "+r.URL.Path)
		hs.mu.Unlock()
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	return hs
}

func TestFetchDumbHTTP(t *testing.T) {
	s := newServer(t)
	hs := s.startDumb(t)
	url := hs.URL + "/src/.git"
	s.fetchScenario(t, url, nil)
	looseObject := regexp.MustCompile(`/objects/[0-9a-f]{2}/[0-9a-f]{38}$`)
	var pack, loose bool
	for _, r := range hs.log() {
		pack = pack || strings.HasSuffix(r, ".pack")
		loose = loose || looseObject.MatchString(r)
		if !strings.HasPrefix(r, "GET ") {
			t.Fatalf("dumb fetch sent %s", r)
		}
	}
	if !pack || !loose {
		t.Fatalf("pack downloaded: %v, loose objects requested: %v\n%s", pack, loose, strings.Join(hs.log(), "\n"))
	}

	// The whole history, across the pack and the loose objects.
	dir, gitDir, db := s.g.target()
	res, err := Fetch(context.Background(), s.g.options(url, gitDir, db, s.ids[4]))
	if err != nil {
		t.Fatal(err)
	}
	s.g.git(dir, "update-ref", "refs/heads/main", s.ids[4].String())
	s.g.verify(dir, gitDir, res, s.ids[4])
	if n := s.g.git(dir, "rev-list", "--count", "main"); n != "5" || len(res.Shallow) != 0 {
		t.Fatalf("full fetch: %s commits, shallow %v", n, res.Shallow)
	}

	// Loose objects are refused over the size limit before they are read.
	_, gitDir, db = s.g.target()
	opts := s.g.options(url, gitDir, db, s.ids[4])
	opts.MaxObjectSize = 20
	if _, err := Fetch(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "object size limit") {
		t.Fatalf("over the limit: %v", err)
	}

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>hello</html>\n")) }))
	defer page.Close()
	_, gitDir, db = s.g.target()
	if _, err := Fetch(context.Background(), s.g.options(page.URL+"/r", gitDir, db, s.ids[4])); err == nil || !strings.Contains(err.Error(), "is this a git repository?") {
		t.Fatalf("web page: %v", err)
	}
}

// Alternates send requests anywhere on the server's word: like redirects,
// they are followed only with http.followRedirects=true.
func TestFetchDumbHTTPAlternates(t *testing.T) {
	s := newServer(t)
	hs := s.startDumb(t)
	fork := filepath.Join(filepath.Dir(s.dir), "fork.git")
	s.g.git("", "clone", "-q", "--bare", "--shared", s.dir, fork)
	s.g.git(fork, "update-server-info")
	info := filepath.Join(fork, "objects", "info")
	os.Remove(filepath.Join(info, "alternates"))
	if err := os.WriteFile(filepath.Join(info, "http-alternates"), []byte("../../src/.git/objects\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	dir, gitDir, db := s.g.target()
	opts := s.g.options(hs.URL+"/fork.git", gitDir, db, s.ids[4])
	opts.Depth = 1
	if _, err := Fetch(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "http.followRedirects") {
		t.Fatalf("alternate followed by default: %v", err)
	}
	opts.Config = s.g.config("[http]\nfollowRedirects = true\n")
	res, err := Fetch(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	s.g.verify(dir, gitDir, res, s.ids[4])
}
