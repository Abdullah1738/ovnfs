package refs

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// verifyRefs runs git's own consistency checks on the repository's refs.
func verifyRefs(t *testing.T, r *gitRepo) {
	t.Helper()
	if out, err := r.tryGit("fsck", "--no-progress"); err != nil {
		t.Errorf("git fsck: %v\n%s", err, out)
	}
	if out, err := r.tryGit("refs", "verify", "--strict"); err != nil && !strings.Contains(out, "usage:") {
		t.Errorf("git refs verify: %v\n%s", err, out)
	}
}

// TestReadLargeReftable reads a table git wrote with many ref blocks and a
// ref index, checking listings and point lookups (index descent, restart
// binary search, misses between and around existing names).
func TestReadLargeReftable(t *testing.T) {
	for _, format := range []object.Format{object.SHA1, object.SHA256} {
		t.Run(format.String(), func(t *testing.T) {
			r := newGitRepo(t, BackendReftable, format)
			c := r.commit("one")
			var in strings.Builder
			const n = 3000
			for i := range n {
				fmt.Fprintf(&in, "create refs/heads/branch/%05d %s\n", i*2, c)
			}
			r.gitStdin(in.String(), "update-ref", "--stdin")
			r.git("pack-refs", "--all")

			s := r.store()
			tables, err := loadStack(r.gitDir, format)
			if err != nil {
				t.Fatal(err)
			}
			if len(tables) != 1 || tables[0].refIndexPos == 0 {
				t.Fatalf("expected one table with a ref index, got %d tables", len(tables))
			}
			listed, err := s.List("refs/heads/branch/")
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Fields(r.git("for-each-ref", "--format=%(refname)", "refs/heads/branch/"))
			var got []string
			for _, ref := range listed {
				got = append(got, ref.Name)
			}
			if !slices.Equal(got, want) || len(got) != n {
				t.Fatalf("List: %d refs, git %d", len(got), len(want))
			}
			for i := -1; i <= 2*n; i += 7 {
				name := fmt.Sprintf("refs/heads/branch/%05d", i)
				ref, err := s.Read(name)
				exists := i >= 0 && i%2 == 0 && i < 2*n
				if exists && (err != nil || ref.ID != c) {
					t.Errorf("Read(%s) = %v, %v", name, ref.ID, err)
				}
				if !exists && err == nil {
					t.Errorf("Read(%s) found a ref git never wrote", name)
				}
			}
			for _, name := range []string{"HEAD", "refs/heads/main", "refs/heads/branch/", "refs/heads/zzz", "refs/a"} {
				_, err := s.Read(name)
				_, gitErr := r.tryGit("rev-parse", "--verify", "-q", "--end-of-options", name)
				if (err == nil) != (gitErr == nil) {
					t.Errorf("Read(%s): %v; git finds it: %v", name, err, gitErr == nil)
				}
			}
		})
	}
}

// TestReflogAfterNoOpUpdateMatchesGit replays history whose last step is git
// updating HEAD's branch to the value it already has: git then logs only to
// HEAD, and in a reftable repository writes a log-only table whose footer
// says log_position 0. Both our reader and our own Update must agree with git.
func TestReflogAfterNoOpUpdateMatchesGit(t *testing.T) {
	matrix(t, func(t *testing.T, backend Backend, format object.Format) {
		r := newGitRepo(t, backend, format)
		c1 := r.commit("one")
		c2 := r.commit("two")
		r.git("branch", "side", c1.String())
		dated := func(seconds int, args ...string) {
			t.Helper()
			env := r.env
			r.env = append(slices.Clip(env), fmt.Sprintf("GIT_COMMITTER_DATE=%d +0000", seconds))
			r.git(args...)
			r.env = env
		}
		dated(1700010700, "symbolic-ref", "-m", "checkout: moving from main to side", "HEAD", "refs/heads/side")
		dated(1700010800, "symbolic-ref", "-m", "checkout: moving from side to main", "HEAD", "refs/heads/main")
		dated(1700010900, "update-ref", "--no-deref", "-m", "checkout: moving from main to "+c1.String(), "HEAD", c1.String())
		dated(1700011000, "symbolic-ref", "-m", "checkout: moving from "+c1.String()+" to main", "HEAD", "refs/heads/main")
		dated(1700011100, "update-ref", "-m", "commit: tip", "refs/heads/main", c2.String())
		s := r.store()
		checkReflogMatches(t, r, s, "HEAD")
		checkReflogMatches(t, r, s, "refs/heads/main")
		if top := gitReflog(r, "HEAD")[0]; !strings.HasSuffix(top, "|commit: tip") {
			t.Fatalf("git's newest HEAD entry is %q; the scenario no longer exercises a no-op update", top)
		}

		// The same no-op through Update, also via HEAD itself.
		for _, name := range []string{"refs/heads/main", "HEAD"} {
			if err := s.Update(name, c2, UpdateOptions{Old: c2, Reflog: &ReflogUpdate{
				Committer: testSignature(1700011200), Message: "noop via " + name, Policy: LogDefault,
			}}); err != nil {
				t.Fatal(err)
			}
			checkReflogMatches(t, r, s, "HEAD")
			checkReflogMatches(t, r, s, "refs/heads/main")
			if top := gitReflog(r, "HEAD")[0]; !strings.HasSuffix(top, "|noop via "+name) {
				t.Errorf("after Update(%s): git's newest HEAD entry is %q", name, top)
			}
		}
		verifyRefs(t, r)
	})
}

// TestWrittenTableReadableByGit appends one table holding enough refs to
// span several aligned ref blocks, and enough log records for several
// restart points, and checks git reads all of it.
func TestWrittenTableReadableByGit(t *testing.T) {
	for _, format := range []object.Format{object.SHA1, object.SHA256} {
		t.Run(format.String(), func(t *testing.T) {
			r := newGitRepo(t, BackendReftable, format)
			c := r.commit("one")
			st, err := lockStack(r.gitDir, format)
			if err != nil {
				t.Fatal(err)
			}
			const n = 1500
			for i := range n {
				name := fmt.Sprintf("refs/heads/w/%04d", i)
				st.refs = append(st.refs, refRecord{name: name, valueType: refValOne, value: c})
				if i%30 == 0 {
					st.logs = append(st.logs, logRecord{name: name, entry: ReflogEntry{
						Old: format.Zero(), New: c, Committer: testSignature(1700000000 + int64(i)), Message: fmt.Sprintf("bulk %d", i),
					}})
				}
			}
			if err := st.commit(format); err != nil {
				t.Fatal(err)
			}
			verifyRefs(t, r)
			want := strings.Fields(r.git("for-each-ref", "--format=%(refname)", "refs/heads/w/"))
			if len(want) != n {
				t.Fatalf("git lists %d refs, want %d", len(want), n)
			}
			s := r.store()
			listed, err := s.List("refs/heads/w/")
			if err != nil || len(listed) != n {
				t.Fatalf("List: %d refs, %v", len(listed), err)
			}
			for i := 0; i < n; i += 30 {
				name := fmt.Sprintf("refs/heads/w/%04d", i)
				checkReflogMatches(t, r, s, name)
			}
		})
	}
}
