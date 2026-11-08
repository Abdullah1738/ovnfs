package fetch

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// The expectations come from running git 2.56 against a logging
// GIT_SSH_COMMAND and a listening socket.
func TestClassifyURLs(t *testing.T) {
	for _, tc := range []struct {
		in                          string
		transport, user, host, port string
		path                        string
	}{
		{"ssh://u@h:2222/~al/r.git", "ssh", "u", "h", "2222", "~al/r.git"},
		{"ssh://h:abc/r", "ssh", "", "h:abc", "", "/r"},
		{"ssh://h:/r", "ssh", "", "h", "", "/r"},
		{"ssh://[::1]:22/r", "ssh", "", "::1", "22", "/r"},
		{"ssh://[h]/r", "ssh", "", "h", "", "/r"},
		{"ssh://u:pw@h/r", "ssh", "u:pw", "h", "", "/r"},
		{"ssh://u%40x@h/r%20s", "ssh", "u@x", "h", "", "/r s"},
		{"ssh://h/%7Eu/r", "ssh", "", "h", "", "~u/r"},
		{"ssh://u@-h/r", "ssh", "u", "-h", "", "/r"},
		{"git+ssh://h/r", "ssh", "", "h", "", "/r"},
		{"ssh+git://h/r", "ssh", "", "h", "", "/r"},
		{"u@[h:2]:p a/r", "ssh", "u", "h", "2", "p a/r"},
		{"[::1]:r", "ssh", "", "::1", "", "r"},
		{"u@[::1]:r", "ssh", "u", "::1", "", "r"},
		{"h:it's/r", "ssh", "", "h", "", "it's/r"},
		{"h:~/r", "ssh", "", "h", "", "~/r"},
		{"h:", "ssh", "", "h", "", ""},
		{"file:/tmp/x", "ssh", "", "file", "", "/tmp/x"},
		{"git://127.0.0.1:19418/~al/r%20x.git", "git", "", "127.0.0.1", "19418", "~al/r x.git"},
		{"git://localhost/r.git/", "git", "", "localhost", "", "/r.git/"},
		{"file:///tmp/s%72c", "file", "", "", "", "/tmp/src"},
		{"file://host/tmp/src", "file", "", "", "", "/tmp/src"},
		{"/abs/path", "file", "", "", "", "/abs/path"},
		{"./foo:bar", "file", "", "", "", "./foo:bar"},
		{"rel/a:b", "file", "", "", "", "rel/a:b"},
		{"plain", "file", "", "", "", "plain"},
	} {
		ep, err := classify(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if ep.transport != tc.transport || ep.user != tc.user || ep.host != tc.host || ep.port != tc.port || ep.path != tc.path {
			t.Errorf("%q = %s user %q host %q port %q path %q; want %s %q %q %q %q",
				tc.in, ep.transport, ep.user, ep.host, ep.port, ep.path, tc.transport, tc.user, tc.host, tc.port, tc.path)
		}
	}
}

func TestGitHostParam(t *testing.T) {
	for in, want := range map[string]string{
		"git://127.0.0.1:19418/r": "127.0.0.1:19418",
		"git://[127.0.0.1]:9/r":   "[127.0.0.1]:9",
		"git://example.com/r":     "example.com",
	} {
		ep, err := classify(in)
		if err != nil || ep.hostParam != want {
			t.Errorf("%s: host=%q %v, want %q", in, ep.hostParam, err, want)
		}
	}
}

func TestClassifyRefuses(t *testing.T) {
	for in, want := range map[string]string{
		"ssh://-oProxy/r": "strange hostname '-oProxy' blocked",
		"ssh://-u@h/r":    "strange hostname '***@h' blocked", // the user part is redacted
		"h:-r":            "strange pathname '-r' blocked",
		"ssh://h":         "no path specified",
	} {
		if _, err := classify(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
}

func TestHTTPEndpointStripsSlash(t *testing.T) {
	ep, err := classify("https://user:pw@example.com:8443/r.git/")
	if err != nil {
		t.Fatal(err)
	}
	if ep.transport != "https" || ep.url.Path != "/r.git" || ep.url.Host != "example.com:8443" {
		t.Fatalf("got %s %s", ep.transport, ep.url)
	}
}

func TestAllowlist(t *testing.T) {
	allowed := []string{"https", "http", "ssh", "git", "file"}
	for _, in := range []string{
		"ext::ssh -i key host %S 'repo'",
		"fd::3,4",
		"foo::https://example.com/r",
		"ftp://example.com/r.git",
		"ftps://example.com/r.git",
		"svn://example.com/r",
		"persistent-https://example.com/r",
	} {
		if _, err := parseEndpoint(in, allowed); !errors.Is(err, errNotAllowed) {
			t.Errorf("%q: %v, want refusal", in, err)
		}
	}
	if _, err := parseEndpoint("ssh://h/r", []string{"https"}); !errors.Is(err, errNotAllowed) {
		t.Errorf("ssh with only https allowed: %v", err)
	}
	for _, in := range []string{"https://h/r", "http://h/r", "ssh://h/r", "h:r", "git://h/r", "file:///r", "/r"} {
		if _, err := parseEndpoint(in, allowed); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestWindowsPaths(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive letters are only local paths on Windows")
	}
	for _, in := range []string{`C:\repo`, "C:/repo", `rel\a:b`} {
		if ep, err := classify(in); err != nil || ep.transport != "file" {
			t.Errorf("%q: %v %v", in, ep, err)
		}
	}
}

func TestRewriteURL(t *testing.T) {
	rules := []insteadOf{
		{base: "git://git.host.xz/", prefix: "host.xz:/path/to/"},
		{base: "git://git.host.xz/", prefix: "work:"},
		{base: "https://short/", prefix: "w"},
		{base: "https://first/", prefix: "dup:"},
		{base: "https://second/", prefix: "dup:"},
	}
	for in, want := range map[string]string{
		"work:repo.git":               "git://git.host.xz/repo.git",
		"host.xz:/path/to/repo.git":   "git://git.host.xz/repo.git",
		"wx":                          "https://short/x",
		"dup:r":                       "https://first/r",
		"https://unrelated.example/r": "https://unrelated.example/r",
	} {
		if got := rewriteURL(in, rules); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	// git quotes "a!b$c d" as 'a'\!'b$c d' in the remote command.
	if got := shellQuote("a!b$c d"); got != `'a'\!'b$c d'` {
		t.Fatalf("got %s", got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("got %s", got)
	}
}
