package rev

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

var historySpecs = []string{
	"HEAD", "HEAD~1", "HEAD~2", "HEAD~3", "HEAD~4", "HEAD^2", "HEAD~1^2", "HEAD~2^2", "HEAD~3^2",
	"HEAD~2^2~1", "HEAD^{/initial}", "HEAD^{/side}", ":/initial", ":/side work", "main~5",
	"origin/main~1", "origin/main~2", "v1.0", "v1.0~1",
}

// comparePairs checks IsAncestor and MergeBase(s) against git for all
// pairs of the given revisions.
func comparePairs(t *testing.T, g *gitRunner, dir string, src *Source, revs []string) {
	t.Helper()
	var names []string
	for _, r := range revs {
		if out, err := g.try(dir, nil, "rev-parse", "--verify", "-q", r+"^{commit}"); err == nil {
			names = append(names, out)
		}
	}
	f := src.Objects.Format()
	for _, a := range names {
		for _, b := range names {
			ida, _ := object.ParseIDFormat(f, a)
			idb, _ := object.ParseIDFormat(f, b)
			_, gitErr := g.try(dir, nil, "merge-base", "--is-ancestor", a, b)
			got, err := IsAncestor(src, ida, idb)
			if err != nil {
				t.Fatalf("IsAncestor: %v", err)
			}
			if got != (gitErr == nil) {
				t.Errorf("IsAncestor(%s, %s) = %v, git %v", a, b, got, gitErr == nil)
			}
			want, _ := g.try(dir, nil, "merge-base", "--all", a, b)
			bases, err := MergeBases(src, ida, idb)
			if err != nil {
				t.Fatalf("MergeBases: %v", err)
			}
			var gotNames []string
			for _, id := range bases {
				gotNames = append(gotNames, id.String())
			}
			if strings.Join(gotNames, "\n") != want {
				t.Errorf("MergeBases(%s, %s) = %v, git %v", a, b, gotNames, strings.Fields(want))
			}
		}
	}
}

func TestShallowClone(t *testing.T) {
	g := newGit(t)
	origin := buildRichRepo(t, g)
	dir := filepath.Join(t.TempDir(), "clone")
	g.run(t.TempDir(), "clone", "-q", "--depth=2", "--no-single-branch", "file://"+filepath.ToSlash(origin), dir)
	src := openSource(t, g, dir)
	if len(src.Shallow) == 0 {
		t.Fatal("clone is not shallow")
	}
	compareResolve(t, g, dir, src, historySpecs)
	comparePairs(t, g, dir, src, []string{"HEAD", "HEAD~1", "origin/side", "origin/side~1", "origin/feature", "v1.0"})
}

func TestReplaceRefs(t *testing.T) {
	g := newGit(t)
	dir := buildRichRepo(t, g)
	g.run(dir, "commit-graph", "write", "--reachable")
	// main~1 now reads as side's tip; main~3 loses its parents.
	g.run(dir, "replace", "main~1", "side")
	g.run(dir, "replace", "--graft", "main~3")
	src := openSource(t, g, dir)
	if !src.Replaced {
		t.Fatal("replace refs not applied")
	}
	compareResolve(t, g, dir, src, historySpecs)
	comparePairs(t, g, dir, src, []string{"HEAD", "HEAD~1", "HEAD~2", "side", "side~1", "feature", "dup", "v1.0"})
}

func TestSymbolicNamesMatchGit(t *testing.T) {
	g := newGit(t)
	dir := buildRichRepo(t, g)
	g.run(dir, "config", "branch.side.remote", "origin")
	g.run(dir, "config", "branch.side.merge", "refs/heads/gone")
	g.run(dir, "tag", "main", "side") // makes "main" ambiguous: heads/main
	specs := []string{
		"HEAD", "@", "main", "heads/main", "refs/heads/main", "side", "feature", "dup", "light",
		"v1.0", "origin", "origin/HEAD", "origin/main", "remotes/origin/side", "@{u}", "@{upstream}",
		"@{U}", "main@{u}", "feature@{u}", "side@{u}", "dup@{u}", "nope@{u}", "@{push}", "main@{push}",
		"@{-1}", "@{-2}", "@{-3}", "HEAD~1", "HEAD@{1}", "main@{1}", "HEAD^{commit}", "main^0",
		"nope", "HEAD:f", ":/fix", g.run(dir, "rev-parse", "HEAD"), "@{-1}@{u}",
	}
	src := openSource(t, g, dir)
	compareNames(t, g, dir, src, specs)
	// Detached HEAD.
	g.run(dir, "update-ref", "--no-deref", "HEAD", "main~1")
	compareNames(t, g, dir, src, []string{"HEAD", "@", "@{u}", "@{push}", "main", "@{-1}"})
	// Unborn branch.
	g.run(dir, "symbolic-ref", "HEAD", "refs/heads/unborn")
	compareNames(t, g, dir, src, []string{"HEAD", "@{u}", "main"})
}

func compareNames(t *testing.T, g *gitRunner, dir string, src *Source, specs []string) {
	t.Helper()
	for _, spec := range specs {
		for _, mode := range []string{"--symbolic-full-name", "--abbrev-ref"} {
			want, gitErr := g.try(dir, nil, "rev-parse", mode, spec)
			var got string
			var err error
			if mode == "--abbrev-ref" {
				got, err = AbbrevRef(src, spec)
			} else {
				got, err = SymbolicFullName(src, spec)
			}
			switch {
			case gitErr != nil && err == nil:
				t.Errorf("%s %q = %q, git fails: %s", mode, spec, got, want)
			case gitErr == nil && err != nil:
				t.Errorf("%s %q: %v, git %q", mode, spec, err, want)
			case gitErr == nil && got != want:
				t.Errorf("%s %q = %q, git %q", mode, spec, got, want)
			}
		}
	}
}
