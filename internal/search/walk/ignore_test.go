package walk

import "testing"

func TestParseIgnoreFileFraming(t *testing.T) {
	// BOM stripped from the first line, CRLF endings, and parsing stops at
	// the first line that is not UTF-8 (as BufRead::lines errors there).
	f := parseIgnore(t.Context(), []byte("\uFEFF*.log\r\n!keep.log\r\n\xff\n*.txt\n"), nil)
	cases := []struct {
		path string
		want matchResult
	}{
		{"x.log", matchIgnore},
		{"keep.log", matchWhitelist},
		{"x.txt", matchNone},
	}
	for _, c := range cases {
		if got := f.match(c.path, false); got != c.want {
			t.Errorf("%s: got %d, want %d", c.path, got, c.want)
		}
	}
	if parseIgnore(t.Context(), []byte("# only a comment\n\n   \n"), nil) != nil {
		t.Error("rule-less file should compile to nil")
	}
}

func TestParseExcludesFile(t *testing.T) {
	cases := []struct {
		config string
		want   string
		ok     bool
	}{
		{"[core]\n\texcludesfile = ~/.gitignore_global\n", "~/.gitignore_global", true},
		{"  ExcludesFile=\"/x/y\"  \n", "/x/y", true},
		{"excludesfile\n= spaced\n", "spaced", true},
		{"excludesfile = first\nexcludesfile = second\n", "first", true},
		{"excludesfile = \"a b\"\n", "", false},
		{"excludesfiles = x\n", "", false},
		{"[core]\n\teditor = vim\n", "", false},
	}
	for _, c := range cases {
		got, ok := parseExcludesFile([]byte(c.config))
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got (%q, %v), want (%q, %v)", c.config, got, ok, c.want, c.ok)
		}
	}
}
