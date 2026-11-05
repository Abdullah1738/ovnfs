package glob

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/pattern"
	"github.com/stencil-hq/ovnfs/internal/search/walk"
)

func memFS(files ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for _, f := range files {
		m[f] = &fstest.MapFile{Data: []byte(f)}
	}
	return m
}

func run(t *testing.T, root string, opts Options) Result {
	t.Helper()
	res, err := Glob(context.Background(), root, opts)
	if err != nil {
		t.Fatalf("Glob(%q): %v", opts.Pattern, err)
	}
	return res
}

func paths(ms []Match) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Path
	}
	return out
}

func wantPaths(t *testing.T, res Result, want ...string) {
	t.Helper()
	if got := paths(res.Matches); !slices.Equal(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
}

// Pruning by depth bound and MayMatchUnder must find exactly what matching
// every walked path would.
func TestPruningNeverHidesMatches(t *testing.T) {
	fs := fsys.FromIOFS(memFS(
		"a/b/c.txt", "a/b/c/d.txt", "a/x", "a/y/x", "a/y/z/x", "ab/c.txt",
		"b/go.go", "b/b/c.txt", "c/b/q/r.txt", "a.go", "x", "src/a/b/c.rs",
		"src/main.rs", "srcx/main.rs", "a/b/c/d/e/f.txt",
	))
	all, err := walk.Collect(context.Background(), "/", walk.Options{FS: fs, Sorted: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pat      string
		anchored bool
	}{
		{"a/*/c.txt", true}, {"a/**/x", true}, {"{a,b}/*.go", true}, {"*", true},
		{"*", false}, {"**", true}, {"a/b", true}, {"[ab]/*", true},
		{"?/b/*", true}, {"a/b/c/d.txt", true}, {"**/c", true}, {"a/**", true},
		{"*/b/**/*.txt", true}, {"src/**/*.rs", true}, {"src*/*.rs", true},
		{"x", false}, {"{a/y,c}/**/x", true}, {"[!a]/b/c.txt", true}, {"a/*/*/*/*/f.txt", true},
	} {
		src := tc.pat
		if !tc.anchored {
			src = "**/" + src
		}
		g, err := pattern.Compile(src, pattern.Options{})
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, e := range all {
			if g.Match(e.Path) {
				want = append(want, e.Path)
			}
		}
		res := run(t, "/", Options{Pattern: tc.pat, Anchored: tc.anchored, Walk: walk.Options{FS: fs}})
		if got := paths(res.Matches); !slices.Equal(got, want) {
			t.Errorf("%q anchored=%v: got %q, want %q", tc.pat, tc.anchored, got, want)
		}
	}
}

func TestAnchoredVersusRecursive(t *testing.T) {
	fs := fsys.FromIOFS(memFS("a.go", "pkg/b.go", "pkg/sub/c.go", "src/d.go", "gox"))
	for _, tc := range []struct {
		pat      string
		anchored bool
		want     []string
	}{
		{"*.go", false, []string{"a.go", "pkg/b.go", "pkg/sub/c.go", "src/d.go"}},
		{"*.go", true, []string{"a.go"}},
		// A '/' anchors the pattern either way.
		{"pkg/*.go", false, []string{"pkg/b.go"}},
		// "**"-prefixed patterns are left alone: "**/" recurses already, and
		// a "**" not followed by '/' is two '*'s confined to one component.
		{"**/sub/*.go", true, []string{"pkg/sub/c.go"}},
		{"**.go", false, []string{"a.go"}},
		// An exact brace union names root entries; a union with wildcards
		// is recursive like any other slash-free pattern.
		{"{a.go,c.go}", false, []string{"a.go"}},
		{"{*.go,c.*}", false, []string{"a.go", "pkg/b.go", "pkg/sub/c.go", "src/d.go"}},
		// Backslashes are separators; unclosed groups are closed.
		{`pkg\sub\*.go`, false, []string{"pkg/sub/c.go"}},
		{"*.{go,rs", false, []string{"a.go", "pkg/b.go", "pkg/sub/c.go", "src/d.go"}},
		// Directories match too.
		{"sub", false, []string{"pkg/sub"}},
		{"*", true, []string{"a.go", "gox", "pkg", "src"}},
	} {
		res := run(t, "/", Options{Pattern: tc.pat, Anchored: tc.anchored, Walk: walk.Options{FS: fs}})
		if got := paths(res.Matches); !slices.Equal(got, tc.want) {
			t.Errorf("%q anchored=%v: got %q, want %q", tc.pat, tc.anchored, got, tc.want)
		}
	}
}

// Ported from pi-walker glob.rs patterns_are_normalised_like_pi.
func TestPatternsAreNormalisedLikePi(t *testing.T) {
	for _, tc := range []struct {
		pat      string
		anchored bool
		want     string
	}{
		{"*.ts", false, "**/*.ts"},
		{"src/*.ts", false, "src/*.ts"},
		{"**/*.rs", false, "**/*.rs"},
		{"*.ts", true, "*.ts"},
		{`src\**\*.ts`, false, "src/**/*.ts"},
		{"*.{ts,tsx,js", false, "**/*.{ts,tsx,js}"},
		{"{a,{b,c}", false, "**/{a,{b,c}}"},
		{"*.{ts,js}", false, "**/*.{ts,js}"},
		{"{alpha.txt,beta.txt}", false, "{alpha.txt,beta.txt}"},
		{"{*.ts,*.tsx}", false, "**/{*.ts,*.tsx}"},
	} {
		if got := buildPattern(tc.pat, tc.anchored); got != tc.want {
			t.Errorf("buildPattern(%q, anchored=%v) = %q, want %q", tc.pat, tc.anchored, got, tc.want)
		}
	}
	if _, err := NewSelector("*.{ts,tsx,js", false); err != nil {
		t.Errorf("unclosed group: %v", err)
	}
}

func file(path string) *walk.Entry { return &walk.Entry{Path: path, Type: fsys.File} }

// Ported from pi-walker glob.rs scope_selects_files_under_matched_directories.
func TestScopeSelectsFilesUnderMatchedDirectories(t *testing.T) {
	scope, err := NewScope("{src,test}", true)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{"src/a/b.rs": true, "test/x.rs": true, "docs/x.rs": false} {
		if got := scope.Matches(file(path)); got != want {
			t.Errorf("{src,test} matches %q = %v, want %v", path, got, want)
		}
	}
	if !scope.Prunes("docs") || scope.Prunes("src") {
		t.Errorf("{src,test}: Prunes(docs)=%v Prunes(src)=%v, want true false", scope.Prunes("docs"), scope.Prunes("src"))
	}
	flat, err := NewScope("dir/*.ts", true)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{"dir/a.ts": true, "dir/sub/a.js": false, "dir/x.ts/inner.js": true} {
		if got := flat.Matches(file(path)); got != want {
			t.Errorf("dir/*.ts matches %q = %v, want %v", path, got, want)
		}
	}
	// Recursive mode, wildcard directories, and a pattern already ending
	// in "**" (left as is).
	for _, tc := range []struct {
		pat  string
		path string
		want bool
	}{
		{"sr?", "src/deep/a.rs", true},
		{"sr?", "x/src/a.rs", true},
		{"packages/*/src", "packages/a/src/x/y.ts", true},
		{"packages/*/src", "packages/a/lib/y.ts", false},
		{"src/**", "src/a/b.rs", true},
		{"*.rs", "a/b.rs", true},
		{"*.rs", "a/b.ts", false},
	} {
		sel, err := NewScope(tc.pat, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := sel.Matches(file(tc.path)); got != tc.want {
			t.Errorf("scope %q matches %q = %v, want %v", tc.pat, tc.path, got, tc.want)
		}
	}
}

// Ported from pi-walker glob.rs exclude_patterns_match_at_any_depth_unless_anchored.
func TestExcludePatternsMatchAtAnyDepthUnlessAnchored(t *testing.T) {
	if ex, err := NewExclude(nil); ex != nil || err != nil {
		t.Errorf("no patterns: %v, %v", ex, err)
	}
	if ex, err := NewExclude([]string{" ", "/"}); ex != nil || err != nil {
		t.Errorf("blank patterns: %v, %v", ex, err)
	}
	ex, err := NewExclude([]string{"vendor/", "*.test.ts", "/gen", "{a,b}", `third\party`})
	if err != nil || ex == nil {
		t.Fatalf("NewExclude: %v, %v", ex, err)
	}
	for _, path := range []string{"vendor", "src/vendor", "src/x.test.ts", "gen", "a", "x/b", "third/party"} {
		if !ex.MatchPath(path) {
			t.Errorf("exclude misses %q", path)
		}
	}
	for _, path := range []string{"src/gen", "vendored", "x.ts", "x/third/party"} {
		if ex.MatchPath(path) {
			t.Errorf("exclude matches %q", path)
		}
	}
	if _, err := NewExclude([]string{"["}); err == nil {
		t.Error("malformed exclude: no error")
	}
}

func TestTypeFilterResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"real/sub"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "real/a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "real/a.txt"), target, target); err != nil {
		t.Fatal(err)
	}
	for link, to := range map[string]string{"filelink": "real/a.txt", "dirlink": "real", "dangling": "missing"} {
		if err := os.Symlink(to, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	type pt struct {
		Path string
		Type fsys.Type
	}
	for _, tc := range []struct {
		typ  fsys.Type
		want []pt
	}{
		{0, []pt{{"dangling", fsys.Symlink}, {"dirlink", fsys.Symlink}, {"filelink", fsys.Symlink}, {"real", fsys.Dir}, {"real/a.txt", fsys.File}, {"real/sub", fsys.Dir}}},
		{fsys.File, []pt{{"filelink", fsys.File}, {"real/a.txt", fsys.File}}},
		{fsys.Dir, []pt{{"dirlink", fsys.Dir}, {"real", fsys.Dir}, {"real/sub", fsys.Dir}}},
		{fsys.Symlink, []pt{{"dangling", fsys.Symlink}, {"dirlink", fsys.Symlink}, {"filelink", fsys.Symlink}}},
	} {
		res := run(t, root, Options{Pattern: "*", Type: tc.typ})
		var got []pt
		for _, m := range res.Matches {
			got = append(got, pt{m.Path, m.Type})
		}
		if !slices.Equal(got, tc.want) || res.Total != len(tc.want) {
			t.Errorf("type %v: got %v (total %d), want %v", tc.typ, got, res.Total, tc.want)
		}
	}

	// Symlinked directories are reported but never descended.
	wantPaths(t, run(t, root, Options{Pattern: "dirlink/*"}))

	// Ranked, a resolved symlink carries its target's size and mtime.
	res := run(t, root, Options{Pattern: "filelink", Type: fsys.File, SortByMtime: true})
	if len(res.Matches) != 1 {
		t.Fatalf("got %v", res.Matches)
	}
	if m := res.Matches[0]; m.Size != 5 || m.Mtime != target.UnixNano() {
		t.Errorf("filelink: size %d mtime %d, want 5 %d", m.Size, m.Mtime, target.UnixNano())
	}
}

func TestSortByMtimeLimitAndTotal(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	m := fstest.MapFS{}
	add := func(name string, at time.Time) {
		m[name] = &fstest.MapFile{Data: []byte(name), ModTime: at}
	}
	add("old.txt", base)
	add("new.txt", base.Add(time.Hour))
	add("mid/b.txt", base.Add(time.Minute))
	// Within one millisecond, ties go to path order whatever the nanoseconds.
	add("mid/a.txt", base.Add(time.Minute+300*time.Microsecond))
	add("mid/c.txt", base.Add(time.Minute+100*time.Microsecond))
	add("skip.md", base.Add(2*time.Hour))
	fs := fsys.FromIOFS(m)
	all := []string{"new.txt", "mid/a.txt", "mid/b.txt", "mid/c.txt", "old.txt"}

	for limit := range 7 {
		res := run(t, "/", Options{Pattern: "*.txt", SortByMtime: true, Limit: limit, Walk: walk.Options{FS: fs}})
		want := all
		if limit > 0 && limit < len(all) {
			want = all[:limit]
		}
		wantPaths(t, res, want...)
		if res.Total != len(all) || res.Truncated != (len(want) < len(all)) {
			t.Errorf("limit %d: total %d truncated %v", limit, res.Total, res.Truncated)
		}
	}
	res := run(t, "/", Options{Pattern: "new.txt", SortByMtime: true, Walk: walk.Options{FS: fs}})
	if got := res.Matches[0]; got.Size != int64(len("new.txt")) || got.Mtime != base.Add(time.Hour).UnixNano() {
		t.Errorf("metadata = %+v", got)
	}
	// Unranked matches carry no metadata.
	res = run(t, "/", Options{Pattern: "new.txt", Walk: walk.Options{FS: fs}})
	if got := res.Matches[0]; got.Size != -1 || got.Mtime != 0 {
		t.Errorf("unranked metadata = %+v", got)
	}
}

func TestPathOrderLimitAndTotal(t *testing.T) {
	fs := fsys.FromIOFS(memFS("b.txt", "a-b.txt", "a/z.txt", "a/b/c.txt", "c.txt"))
	all := []string{"a-b.txt", "a/b/c.txt", "a/z.txt", "b.txt", "c.txt"}
	for limit := range 7 {
		res := run(t, "/", Options{Pattern: "*.txt", Limit: limit, Walk: walk.Options{FS: fs}})
		want := all
		if limit > 0 && limit < len(all) {
			want = all[:limit]
		}
		wantPaths(t, res, want...)
		if res.Total != len(all) || res.Truncated != (len(want) < len(all)) {
			t.Errorf("limit %d: total %d truncated %v", limit, res.Total, res.Truncated)
		}
	}
}

func TestNodeModulesIncludedWhenMentioned(t *testing.T) {
	fs := fsys.FromIOFS(memFS("index.js", "node_modules/pkg/index.js", "app/node_modules/dep/main.js"))
	skip := walk.Options{FS: fs, SkipNodeModules: true}
	wantPaths(t, run(t, "/", Options{Pattern: "*.js", Walk: skip}), "index.js")
	wantPaths(t, run(t, "/", Options{Pattern: "node_modules/**/*.js", Walk: skip}), "node_modules/pkg/index.js")
	wantPaths(t, run(t, "/", Options{Pattern: "**/node_modules/*/*.js", Walk: skip}),
		"app/node_modules/dep/main.js", "node_modules/pkg/index.js")
	// Without pruning requested, node_modules is walked for any pattern.
	wantPaths(t, run(t, "/", Options{Pattern: "*.js", Walk: walk.Options{FS: fs}}),
		"app/node_modules/dep/main.js", "index.js", "node_modules/pkg/index.js")
}

func TestHiddenAndIgnoreFiles(t *testing.T) {
	m := memFS("keep.log", "drop.log", ".env", ".config/x.log", "sub/drop.log", "sub/ok.txt")
	m[".gitignore"] = &fstest.MapFile{Data: []byte("drop.log\n")}
	fs := fsys.FromIOFS(m)
	wantPaths(t, run(t, "/", Options{Pattern: "*.log", Walk: walk.Options{FS: fs, Gitignore: true}}), "keep.log")
	wantPaths(t, run(t, "/", Options{Pattern: "*.log", Walk: walk.Options{FS: fs}}),
		"drop.log", "keep.log", "sub/drop.log")
	wantPaths(t, run(t, "/", Options{Pattern: "*.log", Walk: walk.Options{FS: fs, Hidden: true, Gitignore: true}}),
		".config/x.log", "keep.log")
	// A pattern naming a hidden entry still needs Hidden to see it.
	wantPaths(t, run(t, "/", Options{Pattern: ".env", Walk: walk.Options{FS: fs}}))
	wantPaths(t, run(t, "/", Options{Pattern: ".env", Walk: walk.Options{FS: fs, Hidden: true}}), ".env")
}

func TestWalkMaxDepthStillApplies(t *testing.T) {
	fs := fsys.FromIOFS(memFS("a.txt", "d/b.txt", "d/e/c.txt"))
	wantPaths(t, run(t, "/", Options{Pattern: "*.txt", Walk: walk.Options{FS: fs, MaxDepth: 2}}), "a.txt", "d/b.txt")
	// A pattern bound looser than the caller's keeps the caller's.
	wantPaths(t, run(t, "/", Options{Pattern: "*/*/*.txt", Anchored: true, Walk: walk.Options{FS: fs, MaxDepth: 2}}))
}

func TestErrors(t *testing.T) {
	fs := fsys.FromIOFS(memFS("a.txt"))
	for _, pat := range []string{"", "[ab", "*.{ts,js}}"} {
		if _, err := Glob(context.Background(), "/", Options{Pattern: pat, Walk: walk.Options{FS: fs}}); err == nil {
			t.Errorf("pattern %q: no error", pat)
		}
	}
	if _, err := Glob(context.Background(), "/a.txt", Options{Pattern: "*", Walk: walk.Options{FS: fs}}); err == nil {
		t.Error("file root: no error")
	}
	if _, err := Glob(context.Background(), filepath.Join(t.TempDir(), "missing"), Options{Pattern: "*"}); err == nil {
		t.Error("missing root: no error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Glob(ctx, "/", Options{Pattern: "*", Walk: walk.Options{FS: fs}}); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Errorf("canceled walk: err = %v", err)
	}
}
