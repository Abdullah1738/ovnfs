//go:build (darwin && cgo) || linux || windows

package watch

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func TestBatchScopesUnknownChanges(t *testing.T) {
	a, b := filepath.FromSlash("/tmp/a"), filepath.FromSlash("/tmp/b")
	ra, rb := filepath.FromSlash("/private/tmp/a"), filepath.FromSlash("/private/tmp/b")
	roots := []root{{a, ra, true}, {b, rb, true}}
	got := func(evs ...event) []item { return batch(roots, evs) }
	in := func(dir, name string) string { return filepath.Join(dir, name) }

	if g := got(event{in(ra, "x"), changed}, event{in(ra, "x"), changed}, event{filepath.FromSlash("/private/tmp/c/x"), changed}); !slices.Equal(g, []item{{a, in(a, "x"), false}}) {
		t.Errorf("plain events: %v", g)
	}
	// A rescan deep in a build directory must not widen to the whole root:
	// that makes every checkout of the folder recheck every path it handed out.
	if g := got(event{in(rb, "sub"), rescan}); !slices.Equal(g, []item{{b, in(b, "sub"), true}}) {
		t.Errorf("rescan below b: %v", g)
	}
	if g := got(event{rb, rescan}); !slices.Equal(g, []item{{b, b, true}}) {
		t.Errorf("rescan at b: %v", g)
	}
	if g := got(event{filepath.FromSlash("/private/tmp"), rescan}); len(g) != 2 {
		t.Errorf("rescan above both roots: %v", g)
	}
	if g := got(event{kind: dropped}); len(g) != 2 {
		t.Errorf("dropped: %v", g)
	}
}

// TestDeliverCoalescesQueuedBatches merges the batches queued while fn runs
// into one call, where a tree change absorbs the changes below it: a burst of
// lost events must cost one invalidation, not one each.
func TestDeliverCoalescesQueuedBatches(t *testing.T) {
	a := filepath.FromSlash("/tmp/a")
	in := func(name string) string { return filepath.Join(a, filepath.FromSlash(name)) }
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var calls [][]Change
	var h hub
	h.start(func(cs []Change) {
		mu.Lock()
		calls = append(calls, cs)
		first := len(calls) == 1
		mu.Unlock()
		if first {
			entered <- struct{}{}
			<-release
		}
	})
	defer h.stop()
	h.roots.Store(&[]root{{a, a, true}})

	h.report([]event{{in("first"), changed}})
	<-entered
	h.report([]event{{in("t/x"), changed}})
	h.report([]event{{in("t"), rescan}})
	h.report([]event{{in("t/y"), changed}, {in("s"), changed}})
	close(release)
	if !h.drain() {
		t.Fatal("hub stopped")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []Change{{Path: in("t"), Tree: true}, {Path: in("s")}}
	if len(calls) != 2 || !slices.Equal(calls[1], want) {
		t.Fatalf("calls = %v, want the first then %v", calls, want)
	}
}

func TestOutermostDropsNestedAndDuplicatePaths(t *testing.T) {
	p := filepath.FromSlash
	got := outermost([]string{p("/a/b"), p("/a b"), p("/a"), p("/c"), p("/c"), p("/ab")})
	if want := []string{p("/a"), p("/a b"), p("/ab"), p("/c")}; !slices.Equal(got, want) {
		t.Errorf("outermost = %q, want %q", got, want)
	}
}
