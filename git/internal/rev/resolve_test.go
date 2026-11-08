package rev

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// buildRichRepo creates a repository with merges, branches, remotes,
// lightweight, annotated and nested tags, reflogs with checkouts and many
// objects (so that short object names collide). It returns its directory.
func buildRichRepo(t *testing.T, g *gitRunner, initArgs ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	g.run(t.TempDir(), append([]string{"init", "-q", "-b", "main"}, append(initArgs, dir)...)...)
	var s strings.Builder
	base := int64(1700000000)
	// main: 1 2 3 [4 = merge 3 + side 11] 5 6; side: 10 11 (from 2); feature from 5.
	s.WriteString(importCommit("refs/heads/main", 1, base+100, "initial commit", nil, map[string]string{"f": "one\n", "dir/g": "g\n"}))
	s.WriteString(importCommit("refs/heads/main", 2, base+200, "add feature\n\nlonger body\nfix: inside body", []int{1}, map[string]string{"f": "two\n"}))
	s.WriteString(importCommit("refs/heads/side", 10, base+250, "side work", []int{2}, map[string]string{"s": "side\n"}))
	s.WriteString(importCommit("refs/heads/side", 11, base+260, "side more", []int{10}, map[string]string{"s": "side2\n"}))
	s.WriteString(importCommit("refs/heads/main", 3, base+300, "fix nasty bug", []int{2}, map[string]string{"f": "three\n"}))
	s.WriteString(importCommit("refs/heads/main", 4, base+400, "Merge side", []int{3, 11}, nil))
	s.WriteString(importCommit("refs/heads/main", 5, base+500, "!important change", []int{4}, map[string]string{"dir/h": "h\n"}))
	s.WriteString(importCommit("refs/heads/feature", 20, base+550, "feature start", []int{5}, map[string]string{"x": "x\n"}))
	s.WriteString(importCommit("refs/heads/main", 6, base+600, "latest on main", []int{5}, map[string]string{"f": "six\n"}))
	s.WriteString(importCommit("refs/heads/dup", 30, base+50, "dup branch", nil, map[string]string{"d": "d\n"}))
	for i := range 300 {
		parents := []int{}
		if i > 0 {
			parents = []int{100 + i - 1}
		}
		s.WriteString(importCommit("refs/heads/bulk", 100+i, base+1000+int64(i), fmt.Sprintf("bulk %d", i), parents, map[string]string{"b": fmt.Sprint(i)}))
	}
	fastImport(t, g, dir, s.String())

	blobs := filepath.Join(t.TempDir(), "blobs")
	os.MkdirAll(blobs, 0o755)
	var paths []string
	for i := range 2500 {
		p := filepath.Join(blobs, fmt.Sprint(i))
		os.WriteFile(p, []byte(fmt.Sprintf("blob number %d\n", i)), 0o644)
		paths = append(paths, p)
	}
	g.runInput(dir, strings.Join(paths, "\n")+"\n", "hash-object", "-w", "--stdin-paths")

	date := func(sec int64) []string { return []string{fmt.Sprintf("GIT_COMMITTER_DATE=@%d +0000", sec)} }
	g.runEnv(dir, date(base+700), "tag", "light", "main~4")
	g.runEnv(dir, date(base+700), "tag", "-a", "-m", "release", "v1.0", "main~3")
	g.runEnv(dir, date(base+700), "tag", "-a", "-m", "nested", "v1.0-nested", "v1.0")
	g.runEnv(dir, date(base+700), "tag", "-a", "-m", "tree", "treetag", "main^{tree}")
	g.runEnv(dir, date(base+700), "tag", "-a", "-m", "blob", "blobtag", "main:f")
	g.runEnv(dir, date(base+700), "tag", "dup", "main~1")
	g.run(dir, "update-ref", "refs/remotes/origin/main", "main~1")
	g.run(dir, "update-ref", "refs/remotes/origin/side", "side")
	g.run(dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	g.run(dir, "config", "remote.origin.url", "https://example.com/r.git")
	g.run(dir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	g.run(dir, "config", "branch.main.remote", "origin")
	g.run(dir, "config", "branch.main.merge", "refs/heads/main")
	g.run(dir, "config", "branch.feature.remote", ".")
	g.run(dir, "config", "branch.feature.merge", "refs/heads/main")
	// A branch named like an abbreviated object name: refs win over objects.
	someBlob := g.run(dir, "rev-parse", "main:f")
	g.run(dir, "branch", someBlob[:7], "side")

	// Reflogs: main moves through its history, HEAD switches around.
	g.run(dir, "update-ref", "-d", "refs/heads/main")
	hist := strings.Fields(g.run(dir, "rev-list", "--reverse", "--first-parent", "feature"))
	for i, c := range hist[:5] {
		g.runEnv(dir, date(base+10000+int64(i)*100), "update-ref", "-m", "commit: step", "refs/heads/main", c)
	}
	g.runEnv(dir, date(base+10600), "update-ref", "-m", "commit: latest", "refs/heads/main", g.run(dir, "rev-parse", "feature~1"))
	g.runEnv(dir, date(base+10700), "symbolic-ref", "-m", "checkout: moving from main to side", "HEAD", "refs/heads/side")
	g.runEnv(dir, date(base+10800), "symbolic-ref", "-m", "checkout: moving from side to main", "HEAD", "refs/heads/main")
	detached := g.run(dir, "rev-parse", "main~2")
	g.runEnv(dir, date(base+10900), "update-ref", "--no-deref", "-m", "checkout: moving from main to "+detached, "HEAD", detached)
	g.runEnv(dir, date(base+11000), "symbolic-ref", "-m", "checkout: moving from "+detached+" to main", "HEAD", "refs/heads/main")
	g.runEnv(dir, date(base+11100), "update-ref", "-m", "commit: tip", "refs/heads/main", g.run(dir, "rev-parse", "feature~1"))
	g.run(dir, "read-tree", "main")
	return dir
}

// collidingPrefixes finds 4-digit prefixes shared by several objects,
// keyed by the sorted kinds sharing them ("blob,commit").
func collidingPrefixes(g *gitRunner, dir string) map[string]string {
	out := g.run(dir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype)")
	byPrefix := map[string][]string{}
	for line := range strings.SplitSeq(out, "\n") {
		name, kind, _ := strings.Cut(line, " ")
		byPrefix[name[:4]] = append(byPrefix[name[:4]], kind)
	}
	found := map[string]string{}
	for p, kinds := range byPrefix {
		if len(kinds) < 2 {
			continue
		}
		sort.Strings(kinds)
		key := strings.Join(kinds, ",")
		if prev, ok := found[key]; !ok || p < prev {
			found[key] = p
		}
	}
	return found
}

func revSpecs(g *gitRunner, dir string) []string {
	specs := []string{
		"HEAD", "@", "main", "heads/main", "refs/heads/main", "side", "feature", "dup", "tags/dup",
		"heads/dup", "light", "v1.0", "v1.0-nested", "treetag", "blobtag", "origin", "origin/main",
		"origin/HEAD", "remotes/origin/side", "nope", "refs/heads/nope", "HEAD~", "HEAD~1", "HEAD~2",
		"HEAD~3", "HEAD~10", "HEAD^", "HEAD^0", "HEAD^1", "HEAD^2", "HEAD^3", "main~2^2", "main~2^2~1",
		"main~2^2~2", "main~2^2~5", "HEAD^^", "HEAD^^^", "HEAD~01", "HEAD^01", "HEAD~-1", "HEAD^+1",
		"v1.0^{}", "v1.0^{commit}", "v1.0^{tree}", "v1.0^{tag}", "v1.0^{object}", "v1.0^0", "v1.0~1",
		"v1.0-nested^{}", "v1.0-nested^{tag}", "v1.0-nested^{commit}", "treetag^{tree}", "treetag^{}",
		"treetag^{commit}", "blobtag^{blob}", "blobtag^{}", "blobtag^{tree}", "HEAD^{tree}",
		"HEAD^{blob}", "HEAD^{tag}", "HEAD^{Commit}", "HEAD^{commit", "HEAD^{tree}^{tree}",
		"HEAD^{commit}^{tree}", "HEAD:", "HEAD:f", "HEAD:dir", "HEAD:dir/", "HEAD:dir/g", "HEAD:nope",
		"HEAD:f/x", "HEAD:./f", "HEAD:dir/../f", "v1.0:f", "treetag:f", "main^{tree}:dir/g", ":f",
		":0:f", ":1:f", ":dir/g", ":nope", ":./f", ":/fix nasty", ":/nasty", ":/^fix", ":/fix:",
		":/!-bulk", ":/!!important", ":/!x", ":/", ":/side more", ":/^initial", ":/longer body",
		":/^longer", ":/feature.*longer", ":/body$", ":/(nasty|zzz) bug", ":/BUG", ":/a{1}",
		"HEAD^{/fix nasty}", "HEAD^{/side}", "HEAD^{/}", "HEAD^{/nomatch}", "main^{/^add}:f",
		"HEAD@{0}", "HEAD@{1}", "HEAD@{2}", "HEAD@{5}", "HEAD@{11}", "HEAD@{12}", "HEAD@{100}",
		"main@{0}", "main@{1}", "main@{3}", "main@{6}", "main@{7}", "@{0}", "@{1}", "@{2}",
		"side@{0}", "@{-1}", "@{-2}", "@{-3}", "@{-4}", "@{-1}~1", "@{-2}@{0}", "@{-1}@{0}",
		"@@{0}", "@{u}", "@{upstream}", "@{UPSTREAM}", "@{U}", "main@{u}", "main@{u}~1",
		"main@{u}@{0}", "feature@{u}", "side@{u}", "nope@{u}", "@{push}", "main@{push}",
		"HEAD@{1700010150}", "HEAD@{@1700010150 +0000}", "main@{2023-11-14 22:15:00 +0000}",
		"main@{2023-11-15 01:03:00 +0000}", "main@{2023-11-15 01:09:00 +0000}", "main@{1979-02-26}",
		"main@{1 year ago}", "HEAD@{now}", "main@{yesterday}", "main@{0}@{0}", "HEAD^-", "HEAD^@",
		"HEAD^!", "main..side", "main...side", "HEAD@{-1}", "",
	}
	for _, name := range []string{"main", "v1.0", "main^{tree}", "main:f", "side"} {
		full := g.run(dir, "rev-parse", name)
		specs = append(specs, full, strings.ToUpper(full[:12]), full[:4], full[:7], full[:3],
			"v1.0-3-g"+full[:7], "anything-g"+full[:10])
	}
	someBlob := g.run(dir, "rev-parse", "main:f")
	specs = append(specs, someBlob[:7], someBlob[:7]+"^{commit}")
	for _, p := range collidingPrefixes(g, dir) {
		specs = append(specs, p, p+"^{commit}", p+"^{tree}", p+"^{blob}", p+"^{tag}", p+"~0",
			p+"^0", p+":", p+"^{}", p+"^{object}", p+"0", p+"^{/bulk}")
	}
	return specs
}

func compareResolve(t *testing.T, g *gitRunner, dir string, src *Source, specs []string) {
	t.Helper()
	for _, spec := range specs {
		want, gitErr := g.try(dir, nil, "rev-parse", "--verify", "-q", "--end-of-options", spec)
		got, err := Resolve(src, spec, Options{EndOfOptions: true})
		switch {
		case gitErr != nil && err == nil:
			t.Errorf("Resolve(%q) = %s, git fails", spec, got)
		case gitErr == nil && err != nil:
			t.Errorf("Resolve(%q): %v, git %s", spec, err, want)
		case gitErr == nil && got.String() != want:
			t.Errorf("Resolve(%q) = %s, git %s", spec, got, want)
		}
	}
}

func TestResolveMatchesGit(t *testing.T) {
	g := newGit(t)
	dir := buildRichRepo(t, g)
	src := openSource(t, g, dir)
	compareResolve(t, g, dir, src, revSpecs(g, dir))
}

func TestResolveSHA256Reftable(t *testing.T) {
	g := newGit(t)
	dir := buildRichRepo(t, g, "--object-format=sha256", "--ref-format=reftable")
	src := openSource(t, g, dir)
	if src.Objects.Format() != object.SHA256 {
		t.Fatalf("format %v", src.Objects.Format())
	}
	compareResolve(t, g, dir, src, revSpecs(g, dir))
}

func TestResolveRejectsOptionsUnlessEndOfOptions(t *testing.T) {
	g := newGit(t)
	dir := buildRichRepo(t, g)
	g.run(dir, "update-ref", "refs/heads/-dash", "main")
	src := openSource(t, g, dir)
	if _, err := Resolve(src, "-dash", Options{}); err == nil {
		t.Error("leading '-' accepted without EndOfOptions")
	}
	compareResolve(t, g, dir, src, []string{"-dash", "-dash~1", "-nope"})
}
