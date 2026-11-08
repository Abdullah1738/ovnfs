package refs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// TestLinkedWorktreeMatchesGit checks per-worktree routing in a linked
// worktree: its own HEAD and refs/bisect/, refs/worktree/ refs, shared
// branches, the main-worktree/ and worktrees/<id>/ prefixes, and where an
// update of the worktree's branch logs.
func TestLinkedWorktreeMatchesGit(t *testing.T) {
	for _, backend := range []Backend{BackendFiles, BackendReftable} {
		t.Run(string(backend), func(t *testing.T) {
			r := newGitRepo(t, backend, object.SHA1)
			c1 := r.commit("one")
			r.git("update-ref", "refs/bisect/main-only", "HEAD")
			c2 := r.commit("two")
			wtDir := filepath.Join(t.TempDir(), "wt")
			r.git("worktree", "add", "-q", "-b", "side", wtDir, c1.String())
			wtRun := func(args ...string) (string, string, error) { return runGitIn(wtDir, r.env, args...) }
			mustWt := func(args ...string) string {
				t.Helper()
				out, stderr, err := wtRun(args...)
				if err != nil {
					t.Fatalf("git %s in worktree: %v: %s", strings.Join(args, " "), err, stderr)
				}
				return out
			}
			mustWt("update-ref", "refs/bisect/bad", c2.String())
			mustWt("update-ref", "refs/worktree/mark", c1.String())
			mustWt("update-ref", "ORIG_HEAD", c2.String())

			wtGitDir := filepath.Join(r.gitDir, "worktrees", "wt")
			wt, err := Open(Options{GitDir: wtGitDir, CommonDir: r.gitDir, Format: object.SHA1, Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			main := r.store()
			mainRun := func(args ...string) (string, string, error) { return runGitIn(r.dir, r.env, args...) }

			shortNames := []string{"HEAD", "ORIG_HEAD", "side", "main", "bisect/bad", "mark", "worktree/mark", "main-only"}
			checkReadsMatch(t, wt, wtRun, shortNames)
			checkReadsMatch(t, main, mainRun, shortNames)

			for _, name := range []string{
				"main-worktree/HEAD", "main-worktree/refs/bisect/main-only", "worktrees/wt/HEAD",
				"worktrees/wt/refs/bisect/bad", "worktrees/wt/ORIG_HEAD", "worktrees/nope/HEAD",
				"main-worktree/refs/heads/main",
			} {
				want, gitErr := r.tryGit("rev-parse", "--verify", "-q", name)
				for label, s := range map[string]*Store{"main": main, "worktree": wt} {
					got, err := s.Resolve(name)
					if (gitErr == nil) != (err == nil) || gitErr == nil && got.ID.String() != want {
						t.Errorf("%s: Resolve(%s) = %s, %v; git %q, %v", label, name, got.ID, err, want, gitErr)
					}
				}
			}
			if got, err := main.Symbolic("worktrees/wt/HEAD"); err != nil || got != "refs/heads/side" {
				t.Errorf("Symbolic(worktrees/wt/HEAD) = %q, %v", got, err)
			}

			// Updating the worktree's branch logs to the worktree's HEAD,
			// not the main worktree's.
			mainHeadLog := gitReflog(r, "HEAD")
			if err := wt.Update("refs/heads/side", c2, UpdateOptions{Old: c1, Reflog: &ReflogUpdate{
				Committer: testSignature(1700009999), Message: "move side", Policy: LogDefault,
			}}); err != nil {
				t.Fatal(err)
			}
			wtHeadLog := strings.Split(mustWt("reflog", "show", "--format=%H %gs", "HEAD", "--"), "\n")
			if wtHeadLog[0] != c2.String()+" move side" {
				t.Errorf("worktree HEAD reflog top = %q", wtHeadLog[0])
			}
			if got := gitReflog(r, "HEAD"); !slices.Equal(got, mainHeadLog) {
				t.Errorf("main HEAD reflog changed:\n%q\n%q", got, mainHeadLog)
			}
			entries, err := wt.Reflog("HEAD")
			if err != nil || len(entries) != len(wtHeadLog) || entries[0].Message != "move side" {
				t.Errorf("worktree Reflog(HEAD) = %v, %v; git has %d entries", entries, err, len(wtHeadLog))
			}

			// A per-worktree ref created from the worktree stays there.
			if err := wt.Update("refs/bisect/good", c1, UpdateOptions{Old: object.SHA1.Zero()}); err != nil {
				t.Fatal(err)
			}
			if got := mustWt("rev-parse", "refs/bisect/good"); got != c1.String() {
				t.Errorf("worktree sees refs/bisect/good = %s", got)
			}
			if _, err := r.tryGit("rev-parse", "--verify", "-q", "refs/bisect/good"); err == nil {
				t.Error("main worktree sees the linked worktree's refs/bisect/good")
			}
			if _, err := main.Read("refs/bisect/good"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("main Read(refs/bisect/good) = %v", err)
			}
			checkReadsMatch(t, wt, wtRun, shortNames)
			verifyRefs(t, r)
		})
	}
}
