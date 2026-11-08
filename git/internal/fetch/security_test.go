package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// recorder is an HTTP server logging the Authorization header of every
// request, answering with handle.
type recorder struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newRecorder(t *testing.T, tls bool, handle http.HandlerFunc) *recorder {
	r := &recorder{}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.mu.Unlock()
		handle(w, req)
	})
	if tls {
		r.Server = httptest.NewTLSServer(h)
	} else {
		r.Server = httptest.NewServer(h)
	}
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path+"?"+r.URL.RawQuery, http.StatusFound)
	}
}

// A server redirecting to another host that asks for credentials must never
// receive the credential meant for that host.
func TestCredentialsStayWithTheAskingOrigin(t *testing.T) {
	needScripts(t)
	s := newServer(t)
	victim := s.startHTTP(t, httpConfig{auth: true})
	hostile := newRecorder(t, false, redirectTo(victim.URL))
	logPath := credentialHelper(t, s.g, "p")
	dir, gitDir, db := s.g.target()
	opts := s.g.options(hostile.URL+"/src", gitDir, db, s.ids[4])
	opts.Config = s.g.config("[credential]\nhelper = fake\n")
	res, err := Fetch(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	s.g.verify(dir, gitDir, res, s.ids[4])
	for _, a := range hostile.seen() {
		if a != "" {
			t.Fatalf("redirecting server received Authorization %q", a)
		}
	}
	log, _ := os.ReadFile(logPath)
	if !strings.Contains(string(log), "host="+strings.TrimPrefix(victim.URL, "http://")+"\n") {
		t.Fatalf("credential not asked for the redirect target:\n%s", log)
	}
}

// A password embedded in the URL is for that URL's origin only.
func TestURLCredentialsNotForwardedOnRedirect(t *testing.T) {
	s := newServer(t)
	other := newRecorder(t, false, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	first := newRecorder(t, false, redirectTo(other.URL))
	_, gitDir, db := s.g.target()
	u, _ := url.Parse(first.URL + "/src")
	u.User = url.UserPassword("u", "SECRET")
	_, err := Fetch(context.Background(), s.g.options(u.String(), gitDir, db, s.ids[4]))
	if err == nil {
		t.Fatal("fetch from a 404 succeeded")
	}
	if a := first.seen(); len(a) == 0 || a[0] == "" {
		t.Fatalf("URL credential not sent to its own origin: %q", a)
	}
	for _, a := range other.seen() {
		if a != "" {
			t.Fatalf("redirect target received Authorization %q", a)
		}
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestRedirectFromHTTPSToHTTPRefused(t *testing.T) {
	s := newServer(t)
	plain := newRecorder(t, false, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	secure := newRecorder(t, true, redirectTo(plain.URL))
	s.g.vars["GIT_SSL_NO_VERIFY"] = "1"
	_, gitDir, db := s.g.target()
	u, _ := url.Parse(secure.URL + "/src")
	u.User = url.UserPassword("u", "p")
	_, err := Fetch(context.Background(), s.g.options(u.String(), gitDir, db, s.ids[4]))
	if err == nil || !strings.Contains(err.Error(), "redirect from https to http") {
		t.Fatalf("err = %v", err)
	}
	if n := len(plain.seen()); n != 0 {
		t.Fatalf("plain http server contacted %d times", n)
	}
}

// "<transport>::<address>" always runs git-remote-<transport>, as with git,
// even when the transport is a native one.
func TestHelperSyntaxNeverNative(t *testing.T) {
	s := newServer(t)
	_, gitDir, db := s.g.target()
	for _, raw := range []string{"file::" + s.dir, "ssh::x", "git::x"} {
		opts := s.g.options(raw, gitDir, db, s.ids[0])
		opts.OpenLocal = func(string) (*object.DB, []object.ID, error) {
			t.Fatalf("%s: opened a local repository", raw)
			return nil, nil, nil
		}
		if _, err := Fetch(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "git-remote-") {
			t.Errorf("%s: %v, want a missing remote helper", raw, err)
		}
	}
}

func TestGitURLLimits(t *testing.T) {
	s := newServer(t)
	_, gitDir, db := s.g.target()
	for raw, want := range map[string]string{
		"git://127.0.0.1:1/" + strings.Repeat("a", 70000): "exceeds",
		"git://127.0.0.1:1/a%00host=evil":                 "NUL byte",
		"git://127.0.0.1%00x:1/a":                         "NUL byte",
		"ssh://h/a%00b":                                   "NUL byte",
	} {
		_, err := Fetch(context.Background(), s.g.options(raw, gitDir, db, s.ids[0]))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.40s: %v, want %q", raw, err, want)
		}
	}
	// dialGit itself refuses NULs, whatever built the endpoint.
	if _, err := dialGit(context.Background(), &endpoint{host: "127.0.0.1", port: "1", path: "/a\x00b"}); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Errorf("dialGit with NUL: %v", err)
	}
}

func TestCredentialInjectionRefused(t *testing.T) {
	needScripts(t)
	g := newGitTest(t)
	logPath := credentialHelper(t, g, "p")
	for _, tc := range []struct {
		name, user, path, cfg string
		ok                    bool
	}{
		{name: "newline in user", user: "u\nhost=evil"},
		{name: "NUL in user", user: "u\x00"},
		{name: "CR in user", user: "u\rhost=evil"},
		{name: "CR allowed by protectProtocol=false", user: "u\r", cfg: "[credential]\nprotectProtocol = false\n", ok: true},
		{name: "newline in path", path: "/r\nhost=evil", cfg: "[credential]\nuseHttpPath = true\n"},
	} {
		os.Remove(logPath)
		cfg := g.config("[credential]\nhelper = fake\n" + tc.cfg)
		c := &credential{username: tc.user}
		store, err := newCredentialStore(cfg, environ{g.lookup}, &url.URL{Scheme: "https", Host: "example.com", Path: tc.path}, c)
		if err != nil {
			t.Fatal(err)
		}
		err = store.fill(context.Background(), cfg, c)
		_, ran := os.Stat(logPath)
		if tc.ok {
			if err != nil || ran != nil {
				t.Errorf("%s: %v (helper ran: %v)", tc.name, err, ran == nil)
			}
			continue
		}
		if err == nil || ran == nil {
			t.Errorf("%s: err %v, helper ran: %v", tc.name, err, ran == nil)
		}
	}
	// An askpass prompt built from a poisoned host is refused too.
	store := &credentialStore{env: environ{g.lookup}, protectCR: true}
	if _, err := store.askpass(context.Background(), nil, "Username for 'https://h\nx': "); err == nil || !strings.Contains(err.Error(), "newline") {
		t.Errorf("askpass prompt with newline: %v", err)
	}
}

func TestServerShallowLinesChecked(t *testing.T) {
	g := newGitTest(t)
	srcDir, ids := g.repo(3)
	_, gitDir, db := g.target()
	// The target has commits 1 and 2 (commit 1 shallow); commit 0 is absent.
	if _, err := Local(context.Background(), g.openDB(filepath.Join(srcDir, ".git")), db, ids[2:], LocalOptions{Depth: 2, PackDir: filepath.Join(gitDir, "objects", "pack")}); err != nil {
		t.Fatal(err)
	}
	f := &fetcher{opts: Options{Target: db}, req: request{format: db.Format(), shallow: []object.ID{ids[1], ids[2]}}}
	absent := ids[0]
	for name, su := range map[string]shallowUpdate{
		"unshallow not sent":    {unshallow: []object.ID{absent}},
		"unshallow no parents":  {unshallow: []object.ID{ids[1]}},
		"shallow absent commit": {shallow: []object.ID{absent}},
	} {
		if _, err := f.result(su, ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	res, err := f.result(shallowUpdate{unshallow: []object.ID{ids[2], ids[2]}, shallow: []object.ID{ids[1], ids[2]}}, "")
	if err != nil || len(res.Unshallow) != 1 || len(res.Shallow) != 0 {
		t.Fatalf("valid update: %+v, %v", res, err)
	}

	su := shallowUpdate{limit: 2}
	line := "shallow " + ids[0].String()
	for i := range 3 {
		err := su.parse(line, db.Format())
		if (err != nil) != (i == 2) {
			t.Fatalf("line %d: %v", i, err)
		}
	}
}

func TestErrorsRedactUserinfo(t *testing.T) {
	allowed := []string{"https", "http", "ssh", "git", "file"}
	for _, raw := range []string{
		"ftp://u:SECRET@h/r",
		"ssh://u:SECRET@h",
		"ssh://-u:SECRET@h/r",
		"http://u:SECRET@h:bad port/r",
		"https://u:SECRET@/r",
		"git://u:SECRET@h/a%00",
		"file://user:SECRET@h", // "file://u:..." is drive U: on Windows
	} {
		_, err := parseEndpoint(raw, allowed)
		if err == nil {
			t.Errorf("%s: accepted", raw)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: error leaks the password: %v", raw, err)
		}
	}
	if got := redactURL("user@host:path"); got != "***@host:path" {
		t.Errorf("scp-like: %s", got)
	}
}

func TestMaxObjectSize(t *testing.T) {
	g := newGitTest(t)
	srcDir, ids := g.repo(2)
	src := g.openDB(filepath.Join(srcDir, ".git"))
	_, gitDir, db := g.target()
	pack := filepath.Join(gitDir, "objects", "pack")
	if _, err := Local(context.Background(), src, db, ids[1:], LocalOptions{PackDir: pack, MaxObjectSize: 20}); err == nil || !strings.Contains(err.Error(), "object size limit") {
		t.Fatalf("Local over the limit: %v", err)
	}
	if _, err := Local(context.Background(), src, db, ids[1:], LocalOptions{PackDir: pack}); err != nil {
		t.Fatal(err)
	}
	// The connectivity check refuses big commits and trees before reading.
	if err := checkConnected(context.Background(), db, ids[1:], nil, nil, 20); err == nil || !strings.Contains(err.Error(), "object size limit") {
		t.Fatalf("checkConnected over the limit: %v", err)
	}

	// A received pack declaring larger objects is refused.
	s := newServer(t)
	hs := s.startHTTP(t, httpConfig{})
	_, gitDir, db = s.g.target()
	opts := s.g.options(hs.URL+"/src", gitDir, db, s.ids[4])
	opts.MaxObjectSize = 20
	if _, err := Fetch(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Fetch over the limit: %v", err)
	}
}
