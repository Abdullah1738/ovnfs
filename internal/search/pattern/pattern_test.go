package pattern

import "testing"

// Matching semantics are pinned against the ignore crate in
// walk/rules_test.go; these cases cover what walkers derive for pruning.

func TestMayMatchUnder(t *testing.T) {
	cases := []struct {
		glob, dir string
		want      bool
	}{
		{"src/*.go", "src", true},
		{"src/*.go", "src/sub", false},
		{"src/*.go", "lib", false},
		{"src/**/*.go", "src/a/b", true},
		{"src/**/*.go", "lib", false},
		{"src/**", "src/a/b", true},
		{"src/**", "srcx", false},
		{"**/*.go", "anything/deep", true},
		{"*.go", "any", false},
		{"a/b/c.txt", "a", true},
		{"a/b/c.txt", "a/b", true},
		{"a/b/c.txt", "a/b/c.txt", false},
		{"a/b/c.txt", "a/x", false},
		{"{src,lib}/*.go", "lib", true},
		{"{src,lib}/*.go", "test", false},
		{"s?c/*.go", "src", true},
		{"[st]rc/*.go", "trc", true},
		{"[st]rc/*.go", "arc", false},
		{"**", "x/y", true},
	}
	for _, c := range cases {
		g, err := Compile(c.glob, Options{})
		if err != nil {
			t.Fatal(c.glob, err)
		}
		if got := g.MayMatchUnder(c.dir); got != c.want {
			t.Errorf("MayMatchUnder(%q, %q) = %v, want %v", c.glob, c.dir, got, c.want)
		}
	}
}

func TestMaxDepth(t *testing.T) {
	cases := []struct {
		glob string
		want int
	}{
		{"*.go", 1},
		{"src/*.go", 2},
		{"a/b/c", 3},
		{"{a,b/c}", 2},
		{"**/*.go", 0},
		{"src/**", 0},
		{"a/[!x]", 0}, // a negated class can match '/'
	}
	for _, c := range cases {
		g, err := Compile(c.glob, Options{})
		if err != nil {
			t.Fatal(c.glob, err)
		}
		if got := g.MaxDepth(); got != c.want {
			t.Errorf("MaxDepth(%q) = %d, want %d", c.glob, got, c.want)
		}
	}
}

// Pruning must never hide a match: for every path a glob matches, each
// ancestor directory must report MayMatchUnder and the path must be within
// MaxDepth.
func TestPruningNeverHidesMatches(t *testing.T) {
	globs := []string{"src/*.go", "src/**/*.go", "{a,b/c}/*.md", "a/b/c.txt", "**/x", "s?c/**", "[ab]/[!z]/q"}
	paths := []string{"src/main.go", "src/a/b/c.go", "a/r.md", "b/c/r.md", "a/b/c.txt", "q/w/x", "sxc/1/2", "a/y/q", "b/z/q"}
	for _, gs := range globs {
		g, err := Compile(gs, Options{})
		if err != nil {
			t.Fatal(gs, err)
		}
		for _, p := range paths {
			if !g.Match(p) {
				continue
			}
			depth := 1
			for i := range len(p) {
				if p[i] != '/' {
					continue
				}
				depth++
				if !g.MayMatchUnder(p[:i]) {
					t.Errorf("%q matches %q but MayMatchUnder(%q) is false", gs, p, p[:i])
				}
			}
			if md := g.MaxDepth(); md != 0 && depth > md {
				t.Errorf("%q matches %q at depth %d beyond MaxDepth %d", gs, p, depth, md)
			}
		}
	}
}

// Ported from pi-walker pattern/tests.rs basename_only_and_union.
func TestBasenameOnlyAndUnion(t *testing.T) {
	glob := func(s string) *Glob {
		g, err := Compile(s, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if !glob("**/*.rs").BasenameOnly() || !glob("**/foo*").BasenameOnly() {
		t.Error("recursive base-name globs: BasenameOnly false")
	}
	if glob("*.rs").BasenameOnly() || glob("src/*.rs").BasenameOnly() {
		t.Error("anchored globs: BasenameOnly true")
	}
	u := Union(glob("**/*.rs"), glob("src/*.md"))
	if !u.Match("x.rs") || !u.Match("src/a.md") || u.Match("a.md") {
		t.Error("union matches wrongly")
	}
	if u.BasenameOnly() {
		t.Error("union with a path glob: BasenameOnly true")
	}
	if !u.MatchBase("q/x.rs", "x.rs") {
		t.Error("union MatchBase")
	}
	if all := Union(glob("x"), glob("**")); !all.Match("any/thing") || all.MaxDepth() != 0 {
		t.Error("union with ** must match everything")
	}
}

func TestUnclosedClass(t *testing.T) {
	if _, err := Compile("[abc", Options{}); err == nil {
		t.Error("unclosed class compiled without AllowUnclosedClass")
	}
	g, err := Compile("[abc", Options{AllowUnclosedClass: true})
	if err != nil || !g.Match("[abc") {
		t.Errorf("lenient unclosed class: err=%v", err)
	}
}
