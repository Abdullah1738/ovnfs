package rev

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// buildRandomHistory imports a random history of n commits with merges,
// octopus merges, criss-crosses, several roots, equal timestamps and clock
// skew, one branch per commit. It returns the commits in creation order.
func buildRandomHistory(t *testing.T, g *gitRunner, dir string, n int, seed uint64) []string {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	var s strings.Builder
	dates := make([]int64, n+1)
	for i := 1; i <= n; i++ {
		var parents []int
		switch r := rng.IntN(100); {
		case i == 1 || r < 4: // a new root
		case r < 70:
			parents = []int{max(1, i-1-rng.IntN(min(i-1, 6)))}
		case r < 95:
			a, b := 1+rng.IntN(i-1), 1+rng.IntN(i-1)
			parents = []int{a}
			if b != a {
				parents = append(parents, b)
			}
		default:
			seen := map[int]bool{}
			for range 3 {
				p := 1 + rng.IntN(i-1)
				if !seen[p] {
					seen[p] = true
					parents = append(parents, p)
				}
			}
		}
		when := int64(1700000000 + i*10)
		switch r := rng.IntN(10); {
		case r == 0 && len(parents) > 0:
			when = dates[parents[0]] // same second as a parent
		case r == 1:
			when -= 500 // skewed clock: older than its parents
		case r == 2 && i > 1:
			when = dates[i-1]
		}
		dates[i] = when
		s.WriteString(importCommit(fmt.Sprintf("refs/heads/c%d", i), i, when, fmt.Sprintf("commit %d", i), parents, map[string]string{"f": fmt.Sprint(i)}))
	}
	marks := fastImport(t, g, dir, s.String())
	out := make([]string, n)
	for i := range n {
		out[i] = marks[i+1]
	}
	return out
}

func compareWalks(t *testing.T, g *gitRunner, dir string, src *Source, commits []string, seed uint64) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 7))
	f := src.Objects.Format()
	for range 60 {
		a, b := commits[rng.IntN(len(commits))], commits[rng.IntN(len(commits))]
		ida, _ := object.ParseIDFormat(f, a)
		idb, _ := object.ParseIDFormat(f, b)
		_, gitErr := g.try(dir, nil, "merge-base", "--is-ancestor", a, b)
		got, err := IsAncestor(src, ida, idb)
		if err != nil {
			t.Fatalf("IsAncestor(%s, %s): %v", a, b, err)
		}
		if got != (gitErr == nil) {
			t.Errorf("IsAncestor(%s, %s) = %v, git %v", a, b, got, gitErr == nil)
		}
		want, _ := g.try(dir, nil, "merge-base", "--all", a, b)
		bases, err := MergeBases(src, ida, idb)
		if err != nil {
			t.Fatalf("MergeBases(%s, %s): %v", a, b, err)
		}
		var gotNames []string
		for _, id := range bases {
			gotNames = append(gotNames, id.String())
		}
		if strings.Join(gotNames, "\n") != want {
			t.Errorf("MergeBases(%s, %s) = %v, git %v", a, b, gotNames, strings.Fields(want))
		}
		one, _ := g.try(dir, nil, "merge-base", a, b)
		base, ok, err := MergeBase(src, ida, idb)
		if err != nil {
			t.Fatal(err)
		}
		if ok && base.String() != one || !ok && one != "" {
			t.Errorf("MergeBase(%s, %s) = %s %v, git %q", a, b, base, ok, one)
		}
	}
}

// TestWalksWithAlternateGraph covers a repository borrowing objects (and
// the commit-graph) from an alternate, with its own newer commits outside
// the graph, as an ovnfs checkout's object store does.
func TestWalksWithAlternateGraph(t *testing.T) {
	g := newGit(t)
	origin := filepath.Join(t.TempDir(), "origin")
	g.run(t.TempDir(), "init", "-q", origin)
	commits := buildRandomHistory(t, g, origin, 100, 11)
	g.run(origin, "commit-graph", "write", "--reachable")
	dir := filepath.Join(t.TempDir(), "clone")
	g.run(t.TempDir(), "clone", "-q", "--shared", "--no-checkout", origin, dir)
	tree := g.run(dir, "mktree")
	rng := rand.New(rand.NewPCG(5, 6))
	for i := range 20 {
		args := []string{"commit-tree", tree}
		for range 1 + rng.IntN(2) {
			args = append(args, "-p", commits[rng.IntN(len(commits))])
		}
		date := fmt.Sprintf("GIT_COMMITTER_DATE=@%d +0000", 1700005000+i)
		c, err := g.exec(dir, []string{date}, fmt.Sprintf("new %d\n", i), args...)
		if err != nil {
			t.Fatal(err)
		}
		commits = append(commits, c)
	}
	src := openSource(t, g, dir)
	w := newWalker(src, true)
	if w.graph == nil {
		t.Fatal("alternate's commit-graph not loaded")
	}
	w.close()
	compareWalks(t, g, dir, src, commits, 3)
}

func TestWalksMatchGit(t *testing.T) {
	g := newGit(t)
	for _, tc := range []struct {
		name  string
		graph []string // commit-graph write arguments; nil for none
		extra []string // repository config
	}{
		{name: "no-graph"},
		{name: "graph", graph: []string{"--reachable"}},
		{name: "graph-v1", graph: []string{"--reachable"}, extra: []string{"commitGraph.generationVersion", "1"}},
		{name: "split", graph: []string{"--reachable", "--split"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for seed := range uint64(3) {
				dir := filepath.Join(t.TempDir(), "r")
				g.run(t.TempDir(), "init", "-q", dir)
				for i := 0; i+1 < len(tc.extra); i += 2 {
					g.run(dir, "config", tc.extra[i], tc.extra[i+1])
				}
				commits := buildRandomHistory(t, g, dir, 120, seed)
				if tc.name == "split" {
					// A base layer over part of history, then a tip layer.
					g.runInput(dir, strings.Join(commits[:60], "\n")+"\n", "commit-graph", "write", "--split", "--stdin-commits")
					g.run(dir, "commit-graph", "write", "--reachable", "--split=no-merge")
				} else if tc.graph != nil {
					g.run(dir, append([]string{"commit-graph", "write"}, tc.graph...)...)
				}
				src := openSource(t, g, dir)
				if tc.graph != nil {
					w := newWalker(src, true)
					if w.graph == nil {
						t.Fatal("commit-graph not loaded")
					}
					if tc.name == "split" && len(w.graph.layers) < 2 {
						t.Fatalf("split graph has %d layers", len(w.graph.layers))
					}
					w.close()
				}
				compareWalks(t, g, dir, src, commits, seed)
			}
		})
	}
}
