package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// urlConfigs set http.k from many URL sections; urlQueries are matched
// against each with `git config --get-urlmatch`.
var urlConfigs = [][]string{
	{"", "https://example.com", "https://example.com/foo", "https://user@example.com", "https://*.example.com",
		"https://a.example.com", "https://example.com:8443", "HTTPS://EXAMPLE.com/a%7eb/./c/../d", "https://example.com/"},
	{"https://a.example.com/x/", "https://a.example.com/x", "https://*.*.com", "https://*", "https://user@*.example.com",
		"https://a.example.com:443", "https://a*.example.com", "https://h.", "https://[::1]/", "http://h:80/"},
	{"https://h/a b", "https://h/a%2fb", "https://h/a?x=1", "https://h/a/../b", "https://h//a", "https://us%65r@h/",
		"https://h/../b", "https://H%41/", "https://h/a#f", "https://user:pw@h/p", "https://@h/q", "file:///tmp", "file://h/tmp/X"},
	{"https://h/a/b/..", "https://h/a/%2e%2e/c", "https://h/~", "https://h/é", "https://h/{x}", "https://h/a%3Ab", "https://h:0443/p"},
}

var urlQueries = []string{
	"https://example.com", "https://example.com/", "https://example.com/foo", "https://example.com/foo/",
	"https://example.com/foobar", "https://example.com/foo/bar", "https://user@example.com/foo/bar",
	"https://user@example.com/x", "https://user:pw@example.com/x", "https://other@example.com/x",
	"https://a.example.com/", "https://b.example.com/", "https://x.b.example.com/", "https://example.com:443/",
	"https://example.com:8443/", "http://example.com/", "https://example.com/a~b/d", "https://example.com/a%7Eb/d/e",
	"https://EXAMPLE.COM", "https://a.example.com/x/z", "https://a.example.com/x", "https://localhost/",
	"https://user@a.example.com/", "https://ab.example.com/", "https://h/", "https://h", "https://[::1]:443/",
	"http://h/", "https://h/a%20b", "https://h/a%2Fb", "https://h/a/b", "https://h/a?x=1", "https://h/b",
	"https://h/a", "https://user@h/p/q", "https://h/q", "https://@h/q", "https://USER@h/", "https://ha/",
	"file:///tmp/x", "file://H/tmp/X", "https://h/a/", "https://h/a/c", "https://h/%7e", "https://h/%C3%A9",
	"https://h/%7Bx%7D", "https://h/a:b", "https://h/p", "HTTPS://h:00443/p/", "https://h.", "https://h?q",
	"notaurl", "https://", "https://h:x/", "https://h/%zz", "https://h/../x", "https://a b/", "https://h:65536/",
}

func TestURLMatchMatchesGit(t *testing.T) {
	o := newOracle(t)
	for ci, urls := range urlConfigs {
		var b strings.Builder
		for i, u := range urls {
			if u == "" {
				fmt.Fprintf(&b, "[http]\n\tk = plain%d\n", i)
			} else {
				fmt.Fprintf(&b, "[http %q]\n\tk = %d\n\tother%d = v\n", u, i, i%3)
			}
		}
		b.WriteString("[http]\n\tlate = plain\n\tother1 = plain\n")
		file := o.write(fmt.Sprintf("url%d.cfg", ci), b.String())
		cfg, err := ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range urlQueries {
			res := o.git(o.root, nil, "config", "--file", file, "--get-urlmatch", "http.K", u)
			e, found, err := cfg.URLMatch("HTTP", "k", u)
			switch {
			case res.code == 128:
				if err == nil {
					t.Errorf("config %d, %s: git fails (%s), we found %q", ci, u, strings.TrimSpace(res.stderr), e.Value)
				}
			case err != nil:
				t.Errorf("config %d, %s: git gives %q (exit %d), we fail: %v", ci, u, res.out, res.code, err)
			case res.code == 1 && found:
				t.Errorf("config %d, %s: git finds nothing, we found %q", ci, u, e.Value)
			case res.code == 0 && (!found || e.Value+"\n" != res.out):
				t.Errorf("config %d, %s: git gives %q, we give %q (found %v)", ci, u, strings.TrimSpace(res.out), e.Value, found)
			}
			if err != nil {
				continue
			}

			res = o.git(o.root, nil, "config", "--file", file, "--get-urlmatch", "http", u)
			entries, err := cfg.URLMatchSection("http", u)
			if err != nil {
				t.Fatal(err)
			}
			var got strings.Builder
			for _, e := range entries {
				fmt.Fprintf(&got, "http.%s %s\n", e.Name(), e.Value)
			}
			if got.String() != res.out {
				t.Errorf("config %d, section for %s:\ngit:\n%sours:\n%s", ci, u, res.out, got.String())
			}
		}
	}
}

// TestURLMatchAllCollectsLikeCredentialHelpers checks multi-valued URL
// settings against the helpers `git credential fill` actually runs.
func TestURLMatchAllCollectsLikeCredentialHelpers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper is a shell script")
	}
	o := newOracle(t)
	log := o.path("helpers.log")
	helper := o.write("helper.sh", "#!/bin/sh\necho \"$1\" >> '"+log+"'\n")
	if err := os.Chmod(helper, 0o755); err != nil {
		t.Fatal(err)
	}
	h := func(name string) string { return "\thelper = " + helper + " " + name + "\n" }
	global := o.write("global.cfg", "[credential]\n"+h("plain1")+
		"[credential \"https://example.com\"]\n"+h("host1")+h("host2")+"\tusername = hostuser\n"+
		"[credential \"https://example.com/foo\"]\n"+h("path1")+"\tusername = pathuser\n"+
		"[credential \"https://other.com\"]\n"+h("other")+
		"[credential]\n"+h("plain2")+
		"[credential \"https://*.com\"]\n"+h("wild")+
		"[credential \"https://example.com\"]\n"+h("host3"))
	env := o.with(map[string]string{"GIT_CONFIG_GLOBAL": global, "GIT_TERMINAL_PROMPT": "0", "GIT_ASKPASS": "", "SSH_ASKPASS": ""})
	o.gitStdin(o.root, env, "protocol=https\nhost=example.com\npath=foo/bar\n\n", "-c", "credential.useHttpPath=true", "credential", "fill")
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for line := range strings.Lines(string(data)) {
		want = append(want, strings.TrimSpace(line))
	}

	cfg, err := Load(Options{LookupEnv: lookup(env)})
	if err != nil {
		t.Fatal(err)
	}
	all, err := cfg.URLMatchAll("credential", "helper", "https://example.com/foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range all {
		got = append(got, filepath.Base(strings.Fields(e.Value)[1]))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("helpers %q, git ran %q", got, want)
	}
	if e, ok, _ := cfg.URLMatch("credential", "username", "https://example.com/foo/bar"); !ok || e.Value != "pathuser" {
		t.Fatalf("username = %q, want pathuser", e.Value)
	}
}
