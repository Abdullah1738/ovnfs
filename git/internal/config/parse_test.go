package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// syntaxCases are config files exercising git's parser corners; each is
// compared with `git config --file <f> --list`.
var syntaxCases = []string{
	"[a]\nx = 1\n",
	"[ a ]\nx = 1\n",
	"[a \"S b\"]\nx = 1\n",
	"[a  \"S\"]\nx = 1\n",
	"[a\t\"S\"]\nx = 1\n",
	"[a\"S\"]\nx = 1\n",
	"[a \"S\" ]\nx = 1\n",
	"[a \"S\"x]\nx = 1\n",
	"[a \"S\\\"q\\\\\\t\"]\nx = 1\n",
	"[A.B.C]\nx = 1\n",
	"[A.Bc]\nX = 1\n",
	"[a.]\nx = 1\n",
	"[.a]\nx = 1\n",
	"[a_b]\nx = 1\n",
	"[a.b \"c\"]\nx = 1\n",
	"[a] x = 1\ny\n",
	"[a]x=1\n",
	"[a]\n1x = 1\n",
	"[a]\nx_y = 1\n",
	"[a]\nx-Y = 1\n",
	"[]\nx = 1\n",
	"[a \"\"]\nx = 1\n",
	"[a \"s]\nx = 1\n",
	"[a \"s\"\nx = 1\n",
	"[a \"b\\\nc\"]\n",
	"[a\n",
	"x = 1\n",
	"[a]\n[b]\n",
	"[Sec \"Sub\"]\n[sec \"sub\"]\nKey = v\n",
	"[a]\nx =   v  w \t \n",
	"[a]\nx = \"  v \"  w  \n",
	"[a]\nx = a\"b c\"d\n",
	"[a]\nx = a # c\n",
	"[a]\nx = a ; c\n",
	"[a]\nx = \"a # c\" ; d\n",
	"[a]\nx = a\\\n  b\n",
	"[a]\nx = \"a\\\n  b\"\n",
	"[a]\nx = a \\\n  b \n",
	"[a]\nx = \\n\\t\\b\\\\\\\"\n",
	"[a]\nx = \\a\n",
	"[a]\nx = \"abc\n",
	"[a]\nx = abc\"\n",
	"[a]\nx =\n",
	"[a]\nx\n",
	"[a]\nx ;c\n",
	"[a]\nx;c\n",
	"[a]\nx y\n",
	"[a]\nx = 1\r\n",
	"[a]\r\nx = \"1 \"\r\ny = 2 \\\r\n3\n",
	"\xef\xbb\xbf[a]\nx = 1\n",
	" \xef\xbb\xbf[a]\nx = 1\n",
	"  [a]\n  x = 1\n",
	"[a] # comment\nx=1\n",
	"[a] x = 1 ; c\n",
	"[a]\nx = 1\n\x00y=2\n",
	"[a]\nx = a  \t b\n",
	"[a]\nx = a\\\n",
	"[a]\nx = a\\",
	"[a]\nx = 1",
	"[a]\nx\t=\t1\n",
	"[a]\n=1\n",
	"[a]\n\tX-Y\t\n",
	"[a]\nx==1\n",
	"[a]\nx = a\rb\n",
	"[a]\nx = a \"\" \n",
	"[a]\nx = \"\"  b\n",
	"[a]\nx = \"\" \\\n  b\n",
	"[a]\nx = \"a",
	"[a]\nx = \"a\\",
	"\v[a]\nx=1\n",
	"[a]\nx = \"a\"\\\n  \"b\"\n",
	"[a]\nx = a\\\n#c\n",
	"[a]\nx = \\\n",
	"[a]\nx = 1\n[b\n",
	"[a]\nx = 1\n[b]\ny = \"\n\n",
	"[a]\nx = a\\ \n",
	"[a]\nx\\\n = 1\n",
	"[a]\n x \n = 1\n",
	"[a]\nx = \v1\n",
	"#c\n;c\n[a]#c\nx=1#c\n",
	"[a]\nx = 1\ny = 2\n[a]\nx = 3\n",
	"[a]\nx = \f1\f\n",
	"[a]\n\fx = 1\n",
	"[a \"s\"",
	"[a \"s\"\n",
	"[a]\nx = \"a\\\"\"\n",
}

var badLine = regexp.MustCompile(`bad config line (\d+) in file`)

func TestParseMatchesGit(t *testing.T) {
	o := newOracle(t)
	for i, src := range syntaxCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			file := o.write(fmt.Sprintf("syntax/%d", i), src)
			want, res := o.gitList(o.root, nil, "--file", file)
			cfg, err := ReadFile(file)
			if res.code != 0 {
				m := badLine.FindStringSubmatch(res.stderr)
				if m == nil {
					t.Fatalf("git failed unexpectedly: %s", res.stderr)
				}
				var pe *ParseError
				if !errors.As(err, &pe) {
					t.Fatalf("%q: git rejects it (%s) but ReadFile gave %v", src, m[0], err)
				}
				if strconv.Itoa(pe.Line) != m[1] || pe.File != file {
					t.Fatalf("%q: git says line %s, we say %v", src, m[1], err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: git accepts it, ReadFile: %v", src, err)
			}
			diffLists(t, want, ourList(cfg))
		})
	}
}

func TestMalformedFileErrors(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/config"
	if err := os.WriteFile(file, []byte("[core]\n\tbare = false\n[oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFile(file)
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line != 3 || pe.File != file {
		t.Fatalf("ReadFile = %v, want a ParseError at %s:3", err, file)
	}
	want := "bad config line 3 in file " + file
	if err.Error() != want {
		t.Fatalf("message %q, want %q", err.Error(), want)
	}

	// Load refuses to build a configuration around a broken file too.
	if _, err := Load(Options{GitDir: dir, CommonDir: dir, LookupEnv: lookup(map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": ""})}); !errors.As(err, &pe) {
		t.Fatalf("Load = %v, want ParseError", err)
	}
}
