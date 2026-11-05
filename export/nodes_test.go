package export

import (
	"slices"
	"strings"
	"testing"
)

// pathOf describes a handle, including its retirement state.
func pathOf(t *testing.T, nt *nodeTable, id uint64) string {
	t.Helper()
	p, ok := nt.path(id)
	if !ok {
		return "<stale>"
	}
	return strings.Join(p, "/")
}

// TestHandlesFollowRenames checks subtree moves and replacement retirement.
func TestHandlesFollowRenames(t *testing.T) {
	nt := newNodeTable()
	dir, child := nt.id("a/d"), nt.id("a/d/f")
	victim := nt.id("b/x")

	nt.rename("a/d", "b/x")

	if got := pathOf(t, nt, dir); got != "b/x" {
		t.Fatalf("renamed dir handle → %q", got)
	}
	if got := pathOf(t, nt, child); got != "b/x/f" {
		t.Fatalf("child handle → %q", got)
	}
	if got := pathOf(t, nt, victim); got != "<stale>" {
		t.Fatalf("replaced target handle → %q, want stale", got)
	}
	if nt.id("b/x/f") != child {
		t.Fatal("lookup of new path minted a new id instead of reusing the moved one")
	}
	if nt.id("a/d") == dir {
		t.Fatal("old path still maps to the moved node")
	}
}

// TestRemovedHandlesGoStaleAndNamesGetFreshIDs checks non-reuse of retired ids.
func TestRemovedHandlesGoStaleAndNamesGetFreshIDs(t *testing.T) {
	nt := newNodeTable()
	old := nt.id("x/y")
	nt.remove("x")
	if got := pathOf(t, nt, old); got != "<stale>" {
		t.Fatalf("removed handle → %q", got)
	}
	if nt.id("x/y") == old {
		t.Fatal("recreated path reused a retired id")
	}
	if p, _ := nt.path(1); !slices.Equal(p, []string{}) {
		t.Fatalf("root path = %v", p)
	}
}
