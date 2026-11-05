package grep

import (
	"bytes"
	"context"
	"fmt"
	iofs "io/fs"
	"math/rand/v2"
	"reflect"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/walk"
)

// lines renders matches as "N:text" with context as "N-text", the way the
// expectations below read.
func lines(ms []Match) []string {
	var out []string
	for _, m := range ms {
		for _, c := range m.Before {
			out = append(out, fmt.Sprintf("%d-%s", c.Line, c.Text))
		}
		prefix := ""
		if m.Path != "" {
			prefix = m.Path + ":"
		}
		out = append(out, prefix+strconv.Itoa(m.Line)+":"+m.Text)
		for _, c := range m.After {
			out = append(out, fmt.Sprintf("%d-%s", c.Line, c.Text))
		}
	}
	return out
}

func searchText(t *testing.T, content string, opts SearchOptions) SearchResult {
	t.Helper()
	res, err := Search([]byte(content), opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func expectLines(t *testing.T, content string, opts SearchOptions, want ...string) {
	t.Helper()
	if got := lines(searchText(t, content, opts).Matches); !slices.Equal(got, want) {
		t.Errorf("Search(%q, %+v)\n got %q\nwant %q", content, opts, got, want)
	}
}

// Expectations in these tests were taken from pi's search() native.

func TestDialect(t *testing.T) {
	for _, tc := range []struct {
		content, pattern string
		ignoreCase       bool
		want             []string
	}{
		{"x٣y\nxy\n", `\d`, false, []string{"1:x٣y"}},
		{"éa\n-\n", `^\w+$`, false, []string{"1:éa"}},
		{"x\u2028y\nx\u000by\n", `x\sy`, false, []string{"1:x\u2028y", "2:x\u000by"}},
		{"a1\nb\n", `[^\D]`, false, []string{"1:a1"}},
		{"é-\n--\n", `[^\W-]`, false, []string{"1:é-"}},
		{"ab\nAB\n", `\u{41}\x42`, false, []string{"2:AB"}},
		{"\u212aELVIN\n", "kelvin", true, []string{"1:\u212aELVIN"}},
		{"ſ\n", "S", true, []string{"1:ſ"}},
		{"ẞ\n", "ß", true, []string{"1:ẞ"}},
		{"ﬀ\nFF\n", "ff", true, []string{"2:FF"}},
		{"Fn MAIN()\nfnmain\n", "fn main", true, []string{"1:Fn MAIN()"}},
		{"${x}\n", "${x}", false, nil},                // braces escaped, '$' still anchors
		{"a{b}\n", "a{b}", false, []string{"1:a{b}"}}, // non-repetition braces are literal
		{"fetch(\n", "fetch(", false, []string{"1:fetch("}},
		{"a)b\n", "a)b", false, []string{"1:a)b"}},
		{"a**\n", "a**", false, []string{"1:a**"}}, // no engine accepts it: literal
		{"foo\nbar\n", `foo\nbar`, false, nil},     // lines never contain '\n'
		{"foo\nbar\n", `foo\s`, false, nil},
		{"a\nb\n", `a[^x]b`, false, nil},
		{"foo\n\nbar\n", `^$`, false, []string{"2:"}},
		{"foo\n\n", `^`, false, []string{"1:foo", "2:"}},
		{"foo\nbar", `bar$`, false, []string{"2:bar"}},
		{"foo\r\nbar\r\n", `foo$`, false, nil},
		{"foo\r\nbar\r\n", `bar`, false, []string{"2:bar"}},
		{"a\nb\n", ``, false, []string{"1:a", "2:b"}},
	} {
		expectLines(t, tc.content, SearchOptions{Pattern: tc.pattern, IgnoreCase: tc.ignoreCase}, tc.want...)
	}
}

func TestUnsupportedSyntax(t *testing.T) {
	for _, p := range []string{`a(?=b)`, `(?<!x)y`, `(ab)\1`, `a++`, `[a-z&&[^aeiou]]`, `\<word\>`, `(?x) a b`} {
		if _, err := Search([]byte("ab\n"), SearchOptions{Pattern: p}); err == nil {
			t.Errorf("pattern %q: want an unsupported-syntax error", p)
		}
	}
}

func TestLiteralOption(t *testing.T) {
	expectLines(t, "a.b(c\naxb(c\n", SearchOptions{Pattern: "a.b(c", Literal: true}, "1:a.b(c")
	expectLines(t, "A.B\n", SearchOptions{Pattern: "a.b", Literal: true, IgnoreCase: true}, "1:A.B")
}

// referenceRegexp is Go's regexp built from the same line-mode tree as
// compilePattern: matching one line with it is the reference for the DFA.
func referenceRegexp(t *testing.T, pattern string, ignoreCase bool) *regexp.Regexp {
	t.Helper()
	flags := syntax.ClassNL | syntax.PerlX | syntax.UnicodeGroups
	if ignoreCase {
		flags |= syntax.FoldCase
	}
	tree, err := parseRust(pattern, flags)
	if err != nil {
		t.Fatal(err)
	}
	return regexp.MustCompile(lineMode(tree).Simplify().String())
}

// referenceLines matches each line of content separately with
// referenceRegexp, returning the 1-based numbers of matching lines.
func referenceLines(t *testing.T, pattern string, ignoreCase bool, content []byte) []int {
	t.Helper()
	re := referenceRegexp(t, pattern, ignoreCase)
	var out []int
	for i, l := range bytes.Split(bytes.TrimSuffix(content, []byte("\n")), []byte("\n")) {
		if re.Match(l) {
			out = append(out, i+1)
		}
	}
	return out
}

func matchedLines(c *compiled, content []byte) []int {
	sc := newScanner(c)
	sc.scan(context.Background(), content, scanParams{content: true})
	var out []int
	for _, m := range sc.out {
		out = append(out, m.Line)
	}
	return out
}

// TestPathsAgree checks the prefilter path (literal scan, then the DFA on
// candidate lines) and the DFA-only path against Go's regexp applied line
// by line.
func TestPathsAgree(t *testing.T) {
	var b strings.Builder
	for i := range 500 {
		fmt.Fprintf(&b, "line %d fn main() { impl Foo for Bar } %s\n", i, strings.Repeat("x", i%37))
		if i%7 == 0 {
			b.WriteString("\n")
		}
		if i%11 == 0 {
			fmt.Fprintf(&b, "FN MAIN %d ünïcödé impl  Baz for Qux\n", i)
		}
	}
	content := []byte(b.String())
	for _, tc := range []struct {
		pattern    string
		ignoreCase bool
	}{
		{"fn main", false}, {"fn main", true}, {`impl\s+\w+ for`, false}, {`impl\s+\w+ for`, true},
		{`main|Baz`, false}, {`(fn|FN) (main|MAIN)`, false}, {`ünïcödé`, true}, {`\d+3 `, false},
		{`^line \d*5 `, false}, {`x{30}$`, false}, {`^$`, false}, {`\bfor\b`, false},
		{`\Bain\(`, false}, {`(?i)ÜNÏ\w+`, false}, {`[^\x00-\x7f]+ impl`, false}, {`^.{0,5}$`, false},
	} {
		c, err := compilePattern(tc.pattern, false, tc.ignoreCase, false)
		if err != nil {
			t.Fatal(err)
		}
		want := referenceLines(t, tc.pattern, tc.ignoreCase, content)
		dfaOnly := *c
		dfaOnly.pre, dfaOnly.exact = nil, false
		for name, cc := range map[string]*compiled{"prefilter": c, "dfa": &dfaOnly} {
			if got := matchedLines(cc, content); !slices.Equal(got, want) || len(want) == 0 {
				t.Errorf("%q (i=%v) %s path: %d lines, regexp %d lines", tc.pattern, tc.ignoreCase, name, len(got), len(want))
			}
		}
	}
	// A pattern with more DFA states than the cache holds restarts the
	// cache mid-scan and must still agree.
	var ab bytes.Buffer
	rng := rand.New(rand.NewPCG(1, 2))
	for range 3000 {
		for range 40 {
			ab.WriteByte("ab"[rng.IntN(2)])
		}
		ab.WriteByte('\n')
	}
	c, err := compilePattern(`a[ab]{12}b$`, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := matchedLines(c, ab.Bytes()), referenceLines(t, `a[ab]{12}b$`, false, ab.Bytes()); !slices.Equal(got, want) {
		t.Errorf("state explosion: %d lines, regexp %d lines", len(got), len(want))
	}
	// A literal pattern needs no regexp at all.
	if c, _ := compilePattern("fn main", false, true, false); c.pre == nil || !c.exact {
		t.Errorf("fn main: want an exact prefilter")
	}
	if c, _ := compilePattern(`impl\s+\w+ for`, false, false, false); c.pre == nil || c.exact {
		t.Errorf(`impl\s+\w+ for: want an inexact prefilter`)
	}
}

func TestContext(t *testing.T) {
	const doc = "1\n2\nm\n3\nm\n4\n5\n6\nm\n"
	// Lines are reported once: 4 is after-context of the first m, not
	// before-context of the second; context stops at the next match.
	expectLines(t, doc, SearchOptions{Pattern: "m", Before: 1, After: 1},
		"2-2", "3:m", "4-3", "5:m", "6-4", "8-6", "9:m")
	expectLines(t, doc, SearchOptions{Pattern: "m", Before: 3},
		"1-1", "2-2", "3:m", "4-3", "5:m", "6-4", "7-5", "8-6", "9:m")
	// Skipped matches take their context with them.
	expectLines(t, doc, SearchOptions{Pattern: "m", Before: 2, After: 2, Offset: 1, MaxCount: 1},
		"5:m", "6-4", "7-5")
	// The last kept match's after-context ends at the next (unreported) match.
	expectLines(t, "m\nx\nm\n", SearchOptions{Pattern: "m", After: 5, MaxCount: 1}, "1:m", "2-x")
}

func TestMaxColumns(t *testing.T) {
	for _, tc := range []struct {
		content string
		max     int
		want    string
	}{
		{"abcdefghij", 5, "ab..."},
		{"abcdefghij", 2, "..."},
		{"ééééé", 6, "é..."},
		{"abc", 3, "abc"},
	} {
		res := searchText(t, tc.content+"\n", SearchOptions{Pattern: "a|é", MaxColumns: tc.max})
		m := res.Matches[0]
		if m.Text != tc.want || m.Truncated != (tc.want != tc.content) {
			t.Errorf("%q max %d: got %q truncated=%v, want %q", tc.content, tc.max, m.Text, m.Truncated, tc.want)
		}
	}
	res := searchText(t, "context line\nmatch\n", SearchOptions{Pattern: "match", Before: 1, MaxColumns: 6})
	if got := res.Matches[0].Before[0].Text; got != "con..." {
		t.Errorf("context line: got %q, want it cut too", got)
	}
}

func TestLineText(t *testing.T) {
	expectLines(t, "ab\u00a0 \t\n", SearchOptions{Pattern: "ab"}, "1:ab")
	expectLines(t, "\xff\xff ab\xe2\x82\n", SearchOptions{Pattern: "ab"}, "1:\ufffd\ufffd ab\ufffd")
}

func TestSearchLimits(t *testing.T) {
	const doc = "ab\nab\nab\n"
	for _, tc := range []struct {
		opts  SearchOptions
		lines int
		count int
		limit bool
	}{
		{SearchOptions{Pattern: "ab"}, 3, 3, false},
		{SearchOptions{Pattern: "ab", MaxCount: 2}, 2, 3, true},
		{SearchOptions{Pattern: "ab", MaxCount: 3}, 3, 3, true},
		{SearchOptions{Pattern: "ab", MaxCount: 5}, 3, 3, false},
		{SearchOptions{Pattern: "ab", Offset: 2}, 1, 3, false},
		{SearchOptions{Pattern: "ab", Mode: Count}, 0, 3, false},
	} {
		res := searchText(t, doc, tc.opts)
		if len(res.Matches) != tc.lines || res.MatchCount != tc.count || res.LimitReached != tc.limit {
			t.Errorf("%+v: got %d matches, count %d, limit %v; want %d, %d, %v", tc.opts,
				len(res.Matches), res.MatchCount, res.LimitReached, tc.lines, tc.count, tc.limit)
		}
	}
}

func TestMultiline(t *testing.T) {
	expectLines(t, "foo\nbar\n", SearchOptions{Pattern: `foo\nbar`, Multiline: true}, "1:foo\nbar")
	expectLines(t, "x\nfoo\nbar\n", SearchOptions{Pattern: `^bar`, Multiline: true}, "3:bar")
	expectLines(t, "a\nb\n", SearchOptions{Pattern: `a.b`, Multiline: true})
	expectLines(t, "a\nb\n", SearchOptions{Pattern: `a[^x]b`, Multiline: true}, "1:a\nb")
	// Matches on adjacent lines merge into one block.
	expectLines(t, "a\nb\nc\nd\nb\nc\n", SearchOptions{Pattern: `b\nc|d`, Multiline: true}, "2:b\nc\nd\nb\nc")
	expectLines(t, "a\nb\nc\nd\n", SearchOptions{Pattern: `b\nc`, Multiline: true, Before: 1, After: 1},
		"1-a", "2:b\nc", "4-d")
	if res := searchText(t, "ab\nab\nx\nab\n", SearchOptions{Pattern: "ab", Multiline: true}); res.MatchCount != 2 {
		t.Errorf("adjacent single-line matches: got %d blocks, want 2", res.MatchCount)
	}
}

func TestBinary(t *testing.T) {
	// A NUL in the first 64 KiB: binary, no matches.
	if res := searchText(t, "match\x00\nmatch\n", SearchOptions{Pattern: "match"}); res.MatchCount != 0 {
		t.Errorf("early NUL: got %d matches", res.MatchCount)
	}
	// Past the window, a reported line with a NUL ends the search; lines
	// without one before it stay, and unreported NUL lines do not matter.
	pad := strings.Repeat("padding line\n", 70<<10/13)
	doc := pad + "match 1\nnul \x00 here\nmatch 2\nmatch \x00 3\nmatch 4\n"
	expectLines(t, doc, SearchOptions{Pattern: "match"}, fmt.Sprintf("%d:match 1", 70<<10/13+1), fmt.Sprintf("%d:match 2", 70<<10/13+3))
}

// tree is an in-memory filesystem for Grep tests.
func tree(files map[string]string) walk.Options {
	m := fstest.MapFS{}
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return walk.Options{FS: fsys.FromIOFS(m), Workers: 4}
}

func grepTree(t *testing.T, files map[string]string, opts Options) Result {
	t.Helper()
	opts.Walk = tree(files)
	res, err := Grep(t.Context(), "/", opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func paths(ms []Match) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Path)
	}
	return out
}

func TestFilters(t *testing.T) {
	files := map[string]string{
		"a.rs": "hit\n", "b/c.rs": "hit\n", "b/d/e.rs": "hit\n", "f.ts": "hit\n", "Makefile": "hit\n",
		"g.TOML": "hit\n", "node_modules/x.rs": "hit\n", ".bashrc": "hit\n",
	}
	for _, tc := range []struct {
		opts Options
		want []string
	}{
		{Options{Glob: "*.rs"}, []string{"a.rs", "b/c.rs", "b/d/e.rs"}},
		{Options{Glob: "*.rs", Anchored: true}, []string{"a.rs"}},
		{Options{Glob: "b/*.rs"}, []string{"b/c.rs"}},
		{Options{Glob: "b/**/*.rs"}, []string{"b/c.rs", "b/d/e.rs"}},
		{Options{Glob: "*.{rs,ts}"}, []string{"a.rs", "b/c.rs", "b/d/e.rs", "f.ts"}},
		{Options{Glob: "{e.rs,f.ts}"}, []string{"f.ts"}},
		{Options{Glob: "*.{rs,ts"}, []string{"a.rs", "b/c.rs", "b/d/e.rs", "f.ts"}},
		// Grep's glob is a scope: files under a matched directory are read.
		{Options{Glob: "b"}, []string{"b/c.rs", "b/d/e.rs"}},
		{Options{Glob: "{b,f.ts}"}, []string{"b/c.rs", "b/d/e.rs", "f.ts"}},
		{Options{Glob: "?/d"}, []string{"b/d/e.rs"}},
		{Options{Exclude: []string{"d", "*.ts", "/a.rs"}}, []string{"Makefile", "b/c.rs", "g.TOML"}},
		{Options{Glob: "*.rs", Exclude: []string{"/b/d/"}}, []string{"a.rs", "b/c.rs"}},
		{Options{Glob: "node_modules/*.rs"}, []string{"node_modules/x.rs"}},
		{Options{Type: "rust"}, []string{"a.rs", "b/c.rs", "b/d/e.rs"}},
		{Options{Type: "make"}, []string{"Makefile"}},
		{Options{Type: ".toml"}, []string{"g.TOML"}},
		{Options{Type: "bashrc"}, nil}, // hidden files are not walked here
		{Options{Glob: "*.rs", Type: "ts"}, nil},
	} {
		tc.opts.Pattern = "hit"
		opts := tree(files)
		opts.SkipNodeModules = true
		tc.opts.Walk = opts
		res, err := Grep(t.Context(), "/", tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := paths(res.Matches); !slices.Equal(got, tc.want) {
			t.Errorf("glob %q type %q: got %q, want %q", tc.opts.Glob, tc.opts.Type, got, tc.want)
		}
	}
	for _, bad := range []string{"[", "*.{rs,ts}}"} {
		if _, err := Grep(t.Context(), "/", Options{Pattern: "x", Glob: bad, Walk: tree(files)}); err == nil {
			t.Errorf("malformed glob %q: want an error", bad)
		}
	}
	if _, err := Grep(t.Context(), "/", Options{Pattern: "x", Exclude: []string{"["}, Walk: tree(files)}); err == nil {
		t.Error("malformed exclude: want an error")
	}
}

// An excluded directory is not entered and a scope prunes directories no
// match can be under: neither is listed.
func TestExcludedAndOutOfScopeDirectoriesAreNotEntered(t *testing.T) {
	files := map[string]string{"src/a.rs": "hit\n", "vendor/x/b.rs": "hit\n", "docs/c.rs": "hit\n"}
	for _, tc := range []struct {
		opts Options
		want []string
	}{
		{Options{Exclude: []string{"vendor", "docs"}}, []string{"src/a.rs"}},
		{Options{Glob: "{src,vendor}", Exclude: []string{"x"}}, []string{"src/a.rs"}},
	} {
		m := fstest.MapFS{}
		for name, content := range files {
			m[name] = &fstest.MapFile{Data: []byte(content)}
		}
		rec := &listingFS{MapFS: m}
		tc.opts.Pattern = "hit"
		tc.opts.Walk = walk.Options{FS: fsys.FromIOFS(rec), Workers: 4}
		res, err := Grep(t.Context(), "/", tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := paths(res.Matches); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: got %q, want %q", tc.opts.Exclude, got, tc.want)
		}
		for _, dir := range rec.listed {
			if strings.Contains(dir, "vendor/x") || strings.Contains(dir, "docs") {
				t.Errorf("glob %q exclude %q: listed %q", tc.opts.Glob, tc.opts.Exclude, dir)
			}
		}
	}
}

// listingFS records the directories listed.
type listingFS struct {
	fstest.MapFS
	mu     sync.Mutex
	listed []string
}

func (l *listingFS) ReadDir(name string) ([]iofs.DirEntry, error) {
	l.mu.Lock()
	l.listed = append(l.listed, name)
	l.mu.Unlock()
	return l.MapFS.ReadDir(name)
}

func longLine(t *testing.T, pattern, content string, multiline bool) Match {
	t.Helper()
	res := searchText(t, content, SearchOptions{Pattern: pattern, Multiline: multiline, MaxColumns: 512})
	if len(res.Matches) == 0 {
		t.Fatalf("%q: no match", pattern)
	}
	return res.Matches[0]
}

// Ported from pi-walker grep/tests.rs long_line_match_past_the_budget_shows_a_window_around_it.
func TestLongLineMatchPastTheBudgetShowsAWindowAroundIt(t *testing.T) {
	line := strings.Repeat("a", 12_000) + "deadline [s120]" + strings.Repeat("b", 2_400)
	m := longLine(t, `deadline \[s120\]`, line, false)
	if !m.Truncated || m.Column != 12_001 || len(m.Text) > 512 ||
		!strings.HasPrefix(m.Text, "...a") || !strings.HasSuffix(m.Text, "b...") || !strings.Contains(m.Text, "deadline [s120]") {
		t.Errorf("truncated=%v column=%d len=%d text=%q", m.Truncated, m.Column, len(m.Text), m.Text)
	}
}

// Ported from pi-walker grep/tests.rs long_line_match_near_the_end_cuts_only_the_start.
func TestLongLineMatchNearTheEndCutsOnlyTheStart(t *testing.T) {
	m := longLine(t, "needle", strings.Repeat("a", 2_000)+"needle", false)
	if m.Column != 2_001 || len(m.Text) > 512 || !strings.HasPrefix(m.Text, "...a") || !strings.HasSuffix(m.Text, "aneedle") {
		t.Errorf("column=%d len=%d text=%q", m.Column, len(m.Text), m.Text)
	}
}

// Ported from pi-walker grep/tests.rs long_line_match_inside_the_head_keeps_the_head.
func TestLongLineMatchInsideTheHeadKeepsTheHead(t *testing.T) {
	m := longLine(t, "needle", "xx needle "+strings.Repeat("a", 2_000), false)
	if want := "xx needle " + strings.Repeat("a", 499) + "..."; m.Column != 4 || m.Text != want {
		t.Errorf("column=%d text=%q", m.Column, m.Text)
	}
}

// Ported from pi-walker grep/tests.rs long_line_window_counts_columns_in_characters_and_cuts_on_boundaries.
func TestLongLineWindowCountsColumnsInCharactersAndCutsOnBoundaries(t *testing.T) {
	m := longLine(t, "needle", strings.Repeat("é", 3_000)+"needle"+strings.Repeat("é", 3_000), false)
	if m.Column != 3_001 || len(m.Text) > 512 || !utf8.ValidString(m.Text) ||
		!strings.HasPrefix(m.Text, "...é") || !strings.HasSuffix(m.Text, "é...") || !strings.Contains(m.Text, "needle") {
		t.Errorf("column=%d len=%d text=%q", m.Column, len(m.Text), m.Text)
	}
}

// Ported from pi-walker grep/tests.rs long_line_window_follows_the_scanners_match
// (its look-around cases need syntax the Go engine rejects).
func TestLongLineWindowFollowsTheScannersMatch(t *testing.T) {
	pad := strings.Repeat("a", 12_000)
	for _, tc := range []struct {
		name, pattern, content string
		multiline              bool
	}{
		{"trailing spaces", "needle +$", pad + "needle   \n", false},
		{"line terminator", `needle\n`, pad + "needle\nnext\n", true},
	} {
		m := longLine(t, tc.pattern, tc.content, tc.multiline)
		if !strings.Contains(m.Text, "needle") || m.Column != 12_001 {
			t.Errorf("%s: column %d, text %q", tc.name, m.Column, m.Text)
		}
	}
}

// Ported from pi-walker grep/tests.rs long_line_window_maps_the_match_past_invalid_utf8.
func TestLongLineWindowMapsTheMatchPastInvalidUTF8(t *testing.T) {
	content := strings.Repeat("a", 12_000) + "\xff" + strings.Repeat("c", 2_000) + "needle" + strings.Repeat("b", 2_400)
	m := longLine(t, "needle", content, false)
	if !strings.Contains(m.Text, "needle") || m.Column != 14_002 {
		t.Errorf("column=%d text=%q", m.Column, m.Text)
	}
}

// Ported from pi-walker grep/tests.rs long_line_window_shows_an_empty_match_at_the_end_of_input.
func TestLongLineWindowShowsAnEmptyMatchAtTheEndOfInput(t *testing.T) {
	line := strings.Repeat("a", 5_000)
	for _, tc := range []struct {
		pattern   string
		multiline bool
	}{{"$", false}, {`\z`, false}, {"x|$", false}, {"$", true}, {`\z`, true}} {
		m := longLine(t, tc.pattern, line, tc.multiline)
		if m.Column != 5_001 || !strings.HasPrefix(m.Text, "...a") || !strings.HasSuffix(m.Text, "a") {
			t.Errorf("%q multiline=%v: column=%d text=%q", tc.pattern, tc.multiline, m.Column, m.Text)
		}
	}
}

// Ported from pi-walker render.rs window_matched_line: the window cases on
// their own.
func TestWindowLine(t *testing.T) {
	for _, tc := range []struct {
		line       string
		max        int
		start, end int
		found      bool
		want       string
		column     int
	}{
		{"abcdefghij", 8, 0, 0, false, "abcde...", 0},
		{"abcdefghij", 8, 1, 2, true, "abcde...", 2},
		{"abcdefghijklmnopqrstuvwxyz", 10, 12, 13, true, "...lmno...", 13},
		{"abcdefghijklmnopqrstuvwxyz", 10, 24, 26, true, "...tuvwxyz", 25},
		{"abcdefghijklmnopqrstuvwxyz", 6, 20, 21, true, "abc...", 21},
		{"ééééééééééxéééééééééé", 12, 20, 21, true, "...éxé...", 11},
	} {
		from, to, lead, trail, column := windowLine([]byte(tc.line), tc.max, tc.start, tc.end, tc.found)
		got := tc.line[from:to]
		if lead {
			got = "..." + got
		}
		if trail {
			got += "..."
		}
		if got != tc.want || column != tc.column {
			t.Errorf("windowLine(%q, %d, %d, %d) = %q col %d, want %q col %d", tc.line, tc.max, tc.start, tc.end, got, column, tc.want, tc.column)
		}
	}
}

func TestModesAndLimits(t *testing.T) {
	files := map[string]string{
		"a": "x\nx\nx\n", // 3
		"b": "y\n",       // 0
		"c": "x\n",       // 1
		"d": "x\nx\n",    // 2
	}
	for _, tc := range []struct {
		name  string
		opts  Options
		want  []string // path:line or path#count
		total int
		files int
		limit bool
	}{
		{"content", Options{}, []string{"a:1", "a:2", "a:3", "c:1", "d:1", "d:2"}, 6, 3, false},
		{"max count", Options{MaxCount: 4}, []string{"a:1", "a:2", "a:3", "c:1"}, 4, 2, true},
		{"offset", Options{Offset: 2, MaxCount: 2}, []string{"a:3", "c:1"}, 4, 2, true},
		{"offset only", Options{Offset: 5}, []string{"d:2"}, 6, 3, false},
		{"per file", Options{MaxCountPerFile: 1}, []string{"a:1", "c:1", "d:1"}, 5, 3, true},
		{"per file exact", Options{MaxCountPerFile: 3}, []string{"a:1", "a:2", "a:3", "c:1", "d:1", "d:2"}, 6, 3, false},
		{"per file and max", Options{MaxCountPerFile: 1, MaxCount: 2}, []string{"a:1", "c:1"}, 3, 2, true},
		{"files", Options{Mode: FilesWithMatches}, []string{"a", "c", "d"}, 3, 3, false},
		{"files offset", Options{Mode: FilesWithMatches, Offset: 1, MaxCount: 1}, []string{"c"}, 2, 2, true},
		{"count", Options{Mode: Count}, []string{"a#3", "c#1", "d#2"}, 6, 3, false},
		{"count offset", Options{Mode: Count, Offset: 3, MaxCount: 2}, []string{"c#1", "d#2"}, 6, 3, true},
	} {
		tc.opts.Pattern = "x"
		res := grepTree(t, files, tc.opts)
		var got []string
		for _, m := range res.Matches {
			switch {
			case tc.opts.Mode == Count:
				got = append(got, m.Path+"#"+strconv.Itoa(m.Count))
			case tc.opts.Mode == FilesWithMatches:
				got = append(got, m.Path)
			default:
				got = append(got, m.Path+":"+strconv.Itoa(m.Line))
			}
		}
		if !slices.Equal(got, tc.want) || res.TotalMatches != tc.total || res.FilesWithMatches != tc.files || res.LimitReached != tc.limit {
			t.Errorf("%s: got %q total %d files %d limit %v; want %q total %d files %d limit %v", tc.name,
				got, res.TotalMatches, res.FilesWithMatches, res.LimitReached, tc.want, tc.total, tc.files, tc.limit)
		}
	}
}

func TestOversizedAndBinaryFiles(t *testing.T) {
	big := strings.Repeat("filler\n", 100) + "late hit\n"
	res := grepTree(t, map[string]string{"big": big, "small": "hit\n", "bin": "hit\x00\n"},
		Options{Pattern: "hit", MaxFileSize: 100})
	if got := paths(res.Matches); !slices.Equal(got, []string{"small"}) || res.SkippedOversized != 0 || res.FilesSearched != 3 {
		t.Errorf("got %q oversized %d searched %d; want [small], 0 (it was readable), 3", got, res.SkippedOversized, res.FilesSearched)
	}
	res = grepTree(t, map[string]string{"big": "hit\n" + big}, Options{Pattern: "hit", MaxFileSize: 100})
	if len(res.Matches) != 1 || res.Matches[0].Line != 1 {
		t.Errorf("oversized file: want its window searched, got %+v", res.Matches)
	}
}

func TestFileRoot(t *testing.T) {
	m := fstest.MapFS{"dir/a.rs": {Data: []byte("one\ntwo one\n")}}
	res, err := Grep(t.Context(), "/dir/a.rs", Options{Pattern: "one", Glob: "*.ts", Walk: walk.Options{FS: fsys.FromIOFS(m)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := lines(res.Matches); !slices.Equal(got, []string{"/dir/a.rs:1:one", "/dir/a.rs:2:two one"}) {
		t.Errorf("file root: got %q", got)
	}
	if res, _ := Grep(t.Context(), "/dir/a.rs", Options{Pattern: "one", Type: "ts", Walk: walk.Options{FS: fsys.FromIOFS(m)}}); len(res.Matches) != 0 {
		t.Errorf("file root of another type: got %q", lines(res.Matches))
	}
	if _, err := Grep(t.Context(), "/missing", Options{Pattern: "x", Walk: walk.Options{FS: fsys.FromIOFS(m)}}); err == nil {
		t.Error("missing root: want an error")
	}
}

// TestDeterministic searches many files with many workers: results are in
// path then line order, identical across runs, and a MaxCount search
// returns exactly the prefix of the unlimited one.
func TestDeterministic(t *testing.T) {
	files := map[string]string{}
	for i := range 300 {
		var b strings.Builder
		for j := range i % 9 {
			fmt.Fprintf(&b, "row %d\nneedle %d %d\n", j, i, j)
		}
		files[fmt.Sprintf("d%d/f%03d.txt", i%7, (i*37)%300)] = b.String()
	}
	opts := Options{Pattern: `needle \d+`, Before: 1}
	opts.Walk = tree(files)
	opts.Walk.Workers = 8
	var first Result
	for run := range 3 {
		res, err := Grep(t.Context(), "/", opts)
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			first = res
			sorted := slices.IsSortedFunc(res.Matches, func(a, b Match) int {
				if c := strings.Compare(a.Path, b.Path); c != 0 {
					return c
				}
				return a.Line - b.Line
			})
			if !sorted || len(res.Matches) == 0 {
				t.Fatalf("matches not in path/line order")
			}
			continue
		}
		if !reflect.DeepEqual(res, first) {
			t.Fatalf("run %d differs", run)
		}
	}
	for _, n := range []int{1, 7, 100, len(first.Matches) + 5} {
		limited := opts
		limited.MaxCount, limited.Offset = n, 3
		res, err := Grep(t.Context(), "/", limited)
		if err != nil {
			t.Fatal(err)
		}
		want := first.Matches[3:min(len(first.Matches), 3+n)]
		if !reflect.DeepEqual(res.Matches, want) {
			t.Errorf("max count %d: not the prefix of the unlimited search", n)
		}
	}
}
