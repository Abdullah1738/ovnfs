//go:build (darwin && cgo) || linux || windows

package watch

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestBatchMapsUnknownChangesToRoots(t *testing.T) {
	a, b := filepath.FromSlash("/tmp/a"), filepath.FromSlash("/tmp/b")
	ra, rb := filepath.FromSlash("/private/tmp/a"), filepath.FromSlash("/private/tmp/b")
	roots := []root{{a, ra, true}, {b, rb, true}}
	got := func(evs ...event) []item { return batch(roots, evs) }
	in := func(dir, name string) string { return filepath.Join(dir, name) }

	if g := got(event{in(ra, "x"), changed}, event{in(ra, "x"), changed}, event{filepath.FromSlash("/private/tmp/c/x"), changed}); !slices.Equal(g, []item{{a, in(a, "x")}}) {
		t.Errorf("plain events: %v", g)
	}
	if g := got(event{in(rb, "sub"), rescan}); !slices.Equal(g, []item{{b, b}}) {
		t.Errorf("rescan below b: %v", g)
	}
	if g := got(event{filepath.FromSlash("/private/tmp"), rescan}); len(g) != 2 {
		t.Errorf("rescan above both roots: %v", g)
	}
	if g := got(event{kind: dropped}); len(g) != 2 {
		t.Errorf("dropped: %v", g)
	}
}

func TestOutermostDropsNestedAndDuplicatePaths(t *testing.T) {
	p := filepath.FromSlash
	got := outermost([]string{p("/a/b"), p("/a b"), p("/a"), p("/c"), p("/c"), p("/ab")})
	if want := []string{p("/a"), p("/a b"), p("/ab"), p("/c")}; !slices.Equal(got, want) {
		t.Errorf("outermost = %q, want %q", got, want)
	}
}
