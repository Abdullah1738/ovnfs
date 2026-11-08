package refs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// oracle runs git in one worktree.
type oracle func(args ...string) (stdout, stderr string, err error)

// checkReadsMatch compares every read API against git in the worktree run
// operates on: listing (for-each-ref), resolution and peeling (show-ref -d,
// rev-parse), symbolic refs (symbolic-ref), short-name expansion
// (rev-parse --symbolic-full-name) and abbreviation (rev-parse --abbrev-ref).
func checkReadsMatch(t *testing.T, s *Store, run oracle, shortNames []string) {
	t.Helper()
	mustRun := func(args ...string) string {
		t.Helper()
		out, stderr, err := run(args...)
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr)
		}
		return out
	}

	// Listing, as for-each-ref shows it: symbolic refs with their final
	// target (as symbolic-ref prints it) and resolved value, dangling ones
	// skipped.
	var want []string
	for line := range strings.Lines(mustRun("for-each-ref", "--format=%(refname) %(objectname) %(symref)")) {
		want = append(want, strings.TrimRight(line, " \n"))
	}
	listed, err := s.List("refs/")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range listed {
		id, target := r.ID, ""
		if r.Symbolic() {
			end, err := s.Resolve(r.Name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if target, err = s.Symbolic(r.Name); err != nil {
				t.Fatal(err)
			}
			id = end.ID
		}
		got = append(got, strings.TrimRight(r.Name+" "+id.String()+" "+target, " "))
	}
	if !slices.Equal(got, want) {
		t.Errorf("List:\n got %q\nwant %q", got, want)
	}

	// Resolution of every listed name and the peeled values git knows.
	peeled := map[string]string{}
	for line := range strings.Lines(mustRun("show-ref", "-d")) {
		hex, name, _ := strings.Cut(strings.TrimSpace(line), " ")
		if base, ok := strings.CutSuffix(name, "^{}"); ok {
			peeled[base] = hex
		}
	}
	for _, r := range listed {
		end, err := s.Resolve(r.Name)
		hex, _, gitErr := run("rev-parse", "--verify", "-q", r.Name)
		if gitErr != nil {
			if err == nil {
				t.Errorf("Resolve(%s) = %s, git cannot resolve it", r.Name, end.ID)
			}
			continue
		}
		if err != nil || end.ID.String() != hex {
			t.Errorf("Resolve(%s) = %s, %v; git: %s", r.Name, end.ID, err, hex)
		}
		if !r.Peeled.IsZero() && r.Peeled.String() != peeled[r.Name] {
			t.Errorf("%s peeled = %s, git: %s", r.Name, r.Peeled, peeled[r.Name])
		}
	}

	// HEAD and symbolic-ref.
	head, err := s.Resolve("HEAD")
	if want := mustRun("rev-parse", "HEAD"); err != nil || head.ID.String() != want {
		t.Errorf("Resolve(HEAD) = %v, %v; git %s", head.ID, err, want)
	}
	for _, name := range append([]string{"HEAD"}, symbolicNames(listed)...) {
		target, symErr := s.Symbolic(name)
		want, _, gitErr := run("symbolic-ref", "-q", name)
		switch {
		case gitErr != nil && !errors.Is(symErr, ErrNotSymbolic):
			t.Errorf("Symbolic(%s) = %q, %v; git: not symbolic", name, target, symErr)
		case gitErr == nil && (symErr != nil || target != want):
			t.Errorf("Symbolic(%s) = %q, %v; git: %q", name, target, symErr, want)
		}
	}

	// Short names: git prints the full name the rules pick, followed through
	// symbolic refs, and prints nothing but a warning when the name is
	// ambiguous.
	for _, short := range shortNames {
		full, ambiguous, err := s.Expand(short)
		want, stderr, gitErr := run("rev-parse", "--symbolic-full-name", short)
		gitAmbiguous := strings.Contains(stderr, "refname '"+short+"' is ambiguous")
		if gitErr != nil && !gitAmbiguous {
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Expand(%s) = %q, %v; git: %s", short, full, err, stderr)
			}
			continue
		}
		if err != nil || ambiguous != gitAmbiguous {
			t.Errorf("Expand(%s) = %q, ambiguous %v, %v; git ambiguous %v", short, full, ambiguous, err, gitAmbiguous)
			continue
		}
		if gitAmbiguous {
			continue
		}
		end, err := s.Resolve(full)
		if err != nil || end.Name != want {
			t.Errorf("Expand(%s) resolves to %q, %v; git: %q", short, end.Name, err, want)
		}
	}

	// Abbreviation of every direct ref (git resolves symbolic ones first).
	for _, r := range listed {
		if r.Symbolic() {
			continue
		}
		for _, mode := range []string{"strict", "loose"} {
			got, err := s.Shorten(r.Name, ShortenOptions{Loose: mode == "loose"})
			want := mustRun("rev-parse", "--abbrev-ref="+mode, r.Name)
			if err != nil || got != want {
				t.Errorf("Shorten(%s, %s) = %q, %v; git: %q", r.Name, mode, got, err, want)
			}
		}
	}
}

func symbolicNames(refs []Ref) []string {
	var out []string
	for _, r := range refs {
		if r.Symbolic() {
			out = append(out, r.Name)
		}
	}
	return out
}

// TestReadsMatchGit builds loose, packed and mixed ref sets with git —
// direct, symbolic, dangling, annotated (peeled) and deliberately ambiguous
// refs — and checks every read API against git's view of them.
func TestReadsMatchGit(t *testing.T) {
	matrix(t, func(t *testing.T, backend Backend, format object.Format) {
		r := newGitRepo(t, backend, format)
		r.commit("one")
		r.git("tag", "-a", "-m", "annotated", "v1")
		r.git("tag", "light")
		r.git("tag", "dup")
		r.git("branch", "dup")
		r.git("branch", "feature")
		r.git("branch", "a/b")
		r.git("update-ref", "refs/remotes/a/b", "HEAD")
		r.git("update-ref", "refs/remotes/x", "HEAD")
		r.git("update-ref", "refs/heads/x", "HEAD")
		r.git("update-ref", "refs/z", "HEAD")
		r.git("update-ref", "refs/heads/z", "HEAD")
		r.git("update-ref", "refs/heads/HEAD", "HEAD")
		r.git("update-ref", "refs/heads/tags/q", "HEAD")
		r.git("update-ref", "refs/tags/q", "HEAD")
		r.commit("two")
		r.git("update-ref", "refs/remotes/origin/main", "HEAD~1")
		r.git("update-ref", "refs/heads/origin", "HEAD~1")
		r.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		r.git("update-ref", "refs/remotes/up/HEAD", "HEAD")
		r.git("symbolic-ref", "refs/dangling", "refs/heads/nowhere")
		r.git("symbolic-ref", "refs/chain", "refs/remotes/origin/HEAD")
		shortNames := []string{
			"main", "feature", "v1", "light", "dup", "origin", "origin/main", "up",
			"heads/main", "tags/v1", "remotes/origin/main", "refs/heads/main", "HEAD",
			"x", "z", "a/b", "q", "tags/q", "chain", "dangling", "nowhere", "missing",
		}
		s := r.store()
		run := func(args ...string) (string, string, error) { return runGitIn(r.dir, r.env, args...) }

		t.Run("loose", func(t *testing.T) { checkReadsMatch(t, s, run, shortNames) })
		r.git("pack-refs", "--all")
		t.Run("packed", func(t *testing.T) {
			checkReadsMatch(t, s, run, shortNames)
			// Packing records peeled tags in packed-refs; reftable always
			// records them.
			v1, err := s.Read("refs/tags/v1")
			if want := r.git("rev-parse", "v1^{}"); err != nil || v1.Peeled.String() != want {
				t.Errorf("refs/tags/v1 peeled = %s, %v; want %s", v1.Peeled, err, want)
			}
		})
		r.git("update-ref", "refs/heads/feature", "HEAD~1")
		r.git("update-ref", "refs/heads/fresh", "HEAD")
		r.git("tag", "-a", "-m", "second", "v2")
		t.Run("mixed", func(t *testing.T) { checkReadsMatch(t, s, run, shortNames) })
		r.git("checkout", "-q", "--detach", "HEAD~1")
		t.Run("detached", func(t *testing.T) {
			checkReadsMatch(t, s, run, shortNames)
			if _, err := s.Symbolic("HEAD"); !errors.Is(err, ErrNotSymbolic) {
				t.Errorf("Symbolic(HEAD) on a detached HEAD: %v", err)
			}
		})
		if _, err := s.Read("refs/heads/nowhere"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Read(missing) = %v, want ErrNotExist", err)
		}
		if _, err := s.Resolve("refs/dangling"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Resolve(dangling) = %v, want ErrNotExist", err)
		}
	})
}

// TestSymrefDepthMatchesGit checks the depth at which symbolic reference
// chains stop resolving.
func TestSymrefDepthMatchesGit(t *testing.T) {
	r := newGitRepo(t, BackendFiles, object.SHA1)
	r.commit("one")
	const n = 7
	for i := 1; i < n; i++ {
		r.git("symbolic-ref", "refs/s"+string(rune('0'+i)), "refs/s"+string(rune('0'+i+1)))
	}
	r.git("update-ref", "refs/s"+string(rune('0'+n)), "HEAD")
	s := r.store()
	for i := 1; i <= n; i++ {
		name := "refs/s" + string(rune('0'+i))
		_, gitErr := r.tryGit("rev-parse", "--verify", "-q", name)
		_, err := s.Resolve(name)
		if (gitErr == nil) != (err == nil) {
			t.Errorf("Resolve(%s): %v; git resolves: %v", name, err, gitErr == nil)
		}
		want, gitErr := r.tryGit("symbolic-ref", "-q", name)
		got, err := s.Symbolic(name)
		if (gitErr == nil) != (err == nil) || gitErr == nil && got != want {
			t.Errorf("Symbolic(%s) = %q, %v; git %q, %v", name, got, err, want, gitErr)
		}
	}
}

// TestLooseRefParsingMatchesGit writes odd loose ref files and checks that
// they resolve exactly when git resolves them, to the same value.
func TestLooseRefParsingMatchesGit(t *testing.T) {
	r := newGitRepo(t, BackendFiles, object.SHA1)
	head := r.commit("one").String()
	contents := []string{
		head, head + "\n", head + " trailing\n", head + "\tx\n", head + "x\n",
		strings.ToUpper(head) + "\n", " " + head + "\n", head + "\r\n", head + "\x00junk",
		head[:39] + "\n", "ref: refs/heads/main\n", "ref:refs/heads/main", "ref:  refs/heads/main  \n",
		"ref:\trefs/heads/main\r\n", "ref: refs/heads/main\nextra\n", "ref: HEAD\n", "ref: main\n",
		"ref:\n", "", "garbage\n",
	}
	s := r.store()
	path := filepath.Join(r.gitDir, "refs", "odd")
	for _, c := range contents {
		if err := os.WriteFile(path, []byte(c), 0o666); err != nil {
			t.Fatal(err)
		}
		want, gitErr := r.tryGit("rev-parse", "--verify", "-q", "refs/odd")
		got, err := s.Resolve("refs/odd")
		if (gitErr == nil) != (err == nil) || gitErr == nil && got.ID.String() != want {
			t.Errorf("content %q: Resolve = %s, %v; git %q, %v", c, got.ID, err, want, gitErr)
		}
	}
}
