package refs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

func testSignature(seconds int64) object.Signature {
	return object.Signature{
		Name:  "O V Nfs",
		Email: "ovnfs@example.com",
		When:  time.Unix(seconds, 0).In(time.FixedZone("", -(8*60+45)*60)),
	}
}

// gitReflog returns git's view of name's reflog, newest first, one line per
// entry: new value, committer, raw date and message.
func gitReflog(r *gitRepo, name string) []string {
	r.t.Helper()
	out := r.git("reflog", "show", "--date=raw", "--format=%H|%gn|%ge|%gd|%gs", name, "--")
	var lines []string
	for line := range strings.Lines(out) {
		// %gd is "<name>@{<date>}"; keep the date only, since git spells the
		// name as given.
		line = strings.TrimSuffix(line, "\n")
		if i, j := strings.Index(line, "@{"), strings.Index(line, "}|"); i >= 0 && j > i {
			line = line[:strings.LastIndex(line[:i], "|")+1] + line[i+2:j] + line[j+1:]
		}
		lines = append(lines, line)
	}
	return lines
}

// ourReflog formats Reflog(name) like gitReflog.
func ourReflog(t *testing.T, s *Store, name string) []string {
	t.Helper()
	entries, err := s.Reflog(name)
	if err != nil {
		t.Fatalf("Reflog(%s): %v", name, err)
	}
	var lines []string
	for _, e := range entries {
		_, offset := e.Committer.When.Zone()
		sign := '+'
		if offset < 0 {
			sign, offset = '-', -offset
		}
		lines = append(lines, fmt.Sprintf("%s|%s|%s|%d %c%02d%02d|%s", e.New, e.Committer.Name, e.Committer.Email,
			e.Committer.When.Unix(), sign, offset/3600, offset/60%60, e.Message))
	}
	return lines
}

func checkReflogMatches(t *testing.T, r *gitRepo, s *Store, name string) {
	t.Helper()
	if got, want := ourReflog(t, s, name), gitReflog(r, name); !slices.Equal(got, want) {
		t.Errorf("Reflog(%s):\n got %q\nwant %q", name, got, want)
	}
}

// TestUpdateMatchesGit drives Update through compare-and-swap updates,
// creations, updates through HEAD, no-ops and failures, checking after each
// step that git sees the same values and reflogs and that git fsck is happy.
func TestUpdateMatchesGit(t *testing.T) {
	matrix(t, func(t *testing.T, backend Backend, format object.Format) {
		r := newGitRepo(t, backend, format)
		c1 := r.commit("one")
		c2 := r.commit("two")
		s := r.store()
		logged := func(msg string, policy LogPolicy) *ReflogUpdate {
			return &ReflogUpdate{Committer: testSignature(1700001234), Message: msg, Policy: policy}
		}
		expect := func(name string, want object.ID) {
			t.Helper()
			if got := r.git("rev-parse", "--verify", name); got != want.String() {
				t.Fatalf("git rev-parse %s = %s, want %s", name, got, want)
			}
		}

		// Sequential compare-and-swap updates of the checked-out branch:
		// git appends each to the branch's and HEAD's reflogs.
		cur := c2
		for i := range 4 {
			next := c1
			if cur == c1 {
				next = c2
			}
			if err := s.Update("refs/heads/main", next, UpdateOptions{Old: cur, Reflog: logged(fmt.Sprintf("ovnfs: step %d\nwith  extra   space", i), LogDefault)}); err != nil {
				t.Fatalf("Update step %d: %v", i, err)
			}
			expect("refs/heads/main", next)
			cur = next
		}
		checkReflogMatches(t, r, s, "refs/heads/main")
		checkReflogMatches(t, r, s, "HEAD")
		if top := gitReflog(r, "HEAD")[0]; !strings.HasSuffix(top, "|ovnfs: step 3 with extra space") {
			t.Errorf("HEAD reflog top = %q", top)
		}

		// Through the symbolic ref: HEAD's target changes, both logs record it.
		other := c1
		if cur == c1 {
			other = c2
		}
		if err := s.Update("HEAD", other, UpdateOptions{Old: cur, Reflog: logged("via HEAD", LogDefault)}); err != nil {
			t.Fatal(err)
		}
		expect("refs/heads/main", other)
		if got, _ := s.Symbolic("HEAD"); got != "refs/heads/main" {
			t.Errorf("HEAD now points at %q", got)
		}
		checkReflogMatches(t, r, s, "refs/heads/main")
		checkReflogMatches(t, r, s, "HEAD")

		// A no-op update leaves the branch's reflog alone but, as git does,
		// still logs to HEAD, which points at it.
		mainBefore, headBefore := len(gitReflog(r, "refs/heads/main")), len(gitReflog(r, "HEAD"))
		if err := s.Update("refs/heads/main", other, UpdateOptions{AnyOld: true, Reflog: logged("noop", LogDefault)}); err != nil {
			t.Fatal(err)
		}
		if n := len(gitReflog(r, "refs/heads/main")); n != mainBefore {
			t.Errorf("no-op update changed the branch reflog: %d -> %d entries", mainBefore, n)
		}
		if head := gitReflog(r, "HEAD"); len(head) != headBefore+1 || !strings.HasSuffix(head[0], "|noop") {
			t.Errorf("no-op update: HEAD reflog %d -> %d entries, top %q", headBefore, len(head), head[0])
		}
		checkReflogMatches(t, r, s, "HEAD")
		checkReflogMatches(t, r, s, "refs/heads/main")

		// Compare-and-swap failures leave the ref alone.
		if err := s.Update("refs/heads/main", c1, UpdateOptions{Old: format.Zero()}); !errors.Is(err, ErrMismatch) {
			t.Errorf("create over existing: %v, want ErrMismatch", err)
		}
		wrong := c1
		if other == c1 {
			wrong = c2
		}
		if err := s.Update("refs/heads/main", c1, UpdateOptions{Old: wrong}); !errors.Is(err, ErrMismatch) {
			t.Errorf("stale old value: %v, want ErrMismatch", err)
		}
		expect("refs/heads/main", other)

		// Creation: a ref outside the default logging namespaces gets no
		// reflog unless the policy is "always".
		if err := s.Update("refs/ovnfs/new", c1, UpdateOptions{Old: format.Zero(), Reflog: logged("create", LogDefault)}); err != nil {
			t.Fatal(err)
		}
		expect("refs/ovnfs/new", c1)
		if _, err := s.Reflog("refs/ovnfs/new"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("reflog of refs/ovnfs/new: %v, want none", err)
		}
		if err := s.Update("refs/ovnfs/new", c1, UpdateOptions{Old: format.Zero()}); !errors.Is(err, ErrMismatch) {
			t.Errorf("second create: %v, want ErrMismatch", err)
		}
		if err := s.Update("refs/ovnfs/missing", c1, UpdateOptions{Old: c2}); !errors.Is(err, ErrMismatch) {
			t.Errorf("update of a missing ref: %v, want ErrMismatch", err)
		}
		if err := s.Update("refs/ovnfs/always", c2, UpdateOptions{Old: format.Zero(), Reflog: logged("always", LogAlways)}); err != nil {
			t.Fatal(err)
		}
		checkReflogMatches(t, r, s, "refs/ovnfs/always")
		// An existing reflog is appended to whatever the policy.
		if err := s.Update("refs/ovnfs/always", c1, UpdateOptions{Old: c2, Reflog: logged("existing", LogExisting)}); err != nil {
			t.Fatal(err)
		}
		checkReflogMatches(t, r, s, "refs/ovnfs/always")
		if n := len(gitReflog(r, "refs/ovnfs/always")); n != 2 {
			t.Errorf("refs/ovnfs/always has %d reflog entries, want 2", n)
		}
		if err := s.Update("refs/heads/unlogged", c1, UpdateOptions{Old: format.Zero(), Reflog: logged("x", LogExisting)}); err != nil {
			t.Fatal(err)
		}
		if lines := gitReflog(r, "refs/heads/unlogged"); len(lines) != 0 {
			t.Errorf("LogExisting created a reflog: %q", lines)
		}

		// An annotated tag with its peeled value.
		r.git("tag", "-a", "-m", "annotated", "t1", c1.String())
		tagID := r.id(r.git("rev-parse", "refs/tags/t1"))
		if err := s.Update("refs/tags/t2", tagID, UpdateOptions{Old: format.Zero(), Peeled: c1}); err != nil {
			t.Fatal(err)
		}
		if got := r.git("show-ref", "-d", "refs/tags/t2"); !strings.Contains(got, c1.String()+" refs/tags/t2^{}") {
			t.Errorf("git show-ref -d refs/tags/t2:\n%s", got)
		}

		// Names git refuses.
		if err := s.Update("refs/heads/main/sub", c1, UpdateOptions{AnyOld: true}); err == nil {
			t.Error("created refs/heads/main/sub below an existing ref")
		}
		if err := s.Update("refs/ovnfs", c1, UpdateOptions{AnyOld: true}); err == nil {
			t.Error("created refs/ovnfs above existing refs")
		}
		if err := s.Update("FETCH_HEAD", c1, UpdateOptions{AnyOld: true}); err == nil {
			t.Error("updated FETCH_HEAD")
		}
		if err := s.Update("refs/heads/bad..name", c1, UpdateOptions{AnyOld: true}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("invalid name: %v", err)
		}

		verifyRefs(t, r)

		// git keeps working on top of our updates, and we read its result.
		r.git("update-ref", "-m", "git after ovnfs", "refs/heads/main", c2.String())
		r.git("pack-refs", "--all")
		run := func(args ...string) (string, string, error) { return runGitIn(r.dir, r.env, args...) }
		checkReadsMatch(t, s, run, []string{"main", "new", "t2"})
		checkReflogMatches(t, r, s, "HEAD")
	})
}

// TestUpdatePackedOnlyRef checks that a ref that exists only in packed-refs
// is updated by writing a loose ref, as git does, leaving packed-refs alone.
func TestUpdatePackedOnlyRef(t *testing.T) {
	r := newGitRepo(t, BackendFiles, object.SHA1)
	c1 := r.commit("one")
	c2 := r.commit("two")
	r.git("branch", "side", c1.String())
	r.git("pack-refs", "--all")
	packed, err := os.ReadFile(filepath.Join(r.gitDir, "packed-refs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.gitDir, "refs", "heads", "side")); !os.IsNotExist(err) {
		t.Fatalf("side is still loose: %v", err)
	}
	s := r.store()
	if err := s.Update("refs/heads/side", c2, UpdateOptions{Old: c1}); err != nil {
		t.Fatal(err)
	}
	loose, err := os.ReadFile(filepath.Join(r.gitDir, "refs", "heads", "side"))
	if err != nil || string(loose) != c2.String()+"\n" {
		t.Errorf("loose ref = %q, %v", loose, err)
	}
	if after, _ := os.ReadFile(filepath.Join(r.gitDir, "packed-refs")); string(after) != string(packed) {
		t.Error("packed-refs changed")
	}
	if got := r.git("rev-parse", "side"); got != c2.String() {
		t.Errorf("git rev-parse side = %s", got)
	}
}

// TestUpdateRespectsLocks checks mutual exclusion with git in both
// directions: a lock git would hold makes Update fail with ErrLocked, and a
// lock we hold makes git fail.
func TestUpdateRespectsLocks(t *testing.T) {
	for _, backend := range []Backend{BackendFiles, BackendReftable} {
		t.Run(string(backend), func(t *testing.T) {
			r := newGitRepo(t, backend, object.SHA1)
			c1 := r.commit("one")
			c2 := r.commit("two")
			s := r.store()
			locks := []string{filepath.Join(r.gitDir, "reftable", "tables.list")}
			if backend == BackendFiles {
				// Updating the checked-out branch locks HEAD too.
				locks = []string{filepath.Join(r.gitDir, "refs", "heads", "main"), filepath.Join(r.gitDir, "HEAD")}
			}
			for _, target := range locks {
				if err := os.WriteFile(target+".lock", nil, 0o666); err != nil {
					t.Fatal(err)
				}
				if err := s.Update("refs/heads/main", c1, UpdateOptions{Old: c2}); !errors.Is(err, ErrLocked) {
					t.Errorf("with %s.lock held: %v, want ErrLocked", filepath.Base(target), err)
				}
				if err := os.Remove(target + ".lock"); err != nil {
					t.Fatal(err)
				}
				if got := r.git("rev-parse", "main"); got != c2.String() {
					t.Fatalf("main moved to %s while locked", got)
				}
			}

			lock, err := createLock(locks[0], lockOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if out, err := r.tryGit("update-ref", "refs/heads/main", c1.String()); err == nil {
				t.Errorf("git update-ref succeeded while we held %s.lock: %s", locks[0], out)
			}
			lock.rollback()
			r.git("update-ref", "refs/heads/main", c1.String())
		})
	}
}

// TestConcurrentUpdates races compare-and-swap updates of one ref from many
// goroutines: every success must be recorded exactly once, in a reflog whose
// entries chain old to new, and the repository must stay valid for git.
func TestConcurrentUpdates(t *testing.T) {
	for _, backend := range []Backend{BackendFiles, BackendReftable} {
		t.Run(string(backend), func(t *testing.T) {
			r := newGitRepo(t, backend, object.SHA1)
			ids := []object.ID{r.commit("one"), r.commit("two"), r.commit("three")}
			s := r.store()
			if err := s.Update("refs/heads/race", ids[0], UpdateOptions{Old: object.SHA1.Zero(), Reflog: &ReflogUpdate{Committer: testSignature(1), Message: "start", Policy: LogDefault}}); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			successes := 0
			for w := range 8 {
				wg.Go(func() {
					for i := range 10 {
						cur, err := s.Resolve("refs/heads/race")
						if err != nil {
							t.Error(err)
							return
						}
						next := ids[(slices.Index(ids, cur.ID)+1)%len(ids)]
						err = s.Update("refs/heads/race", next, UpdateOptions{Old: cur.ID, Reflog: &ReflogUpdate{
							Committer: testSignature(int64(w*100 + i)), Message: "race", Policy: LogDefault,
						}})
						switch {
						case err == nil:
							mu.Lock()
							successes++
							mu.Unlock()
						case errors.Is(err, ErrLocked), errors.Is(err, ErrMismatch):
						default:
							t.Error(err)
						}
					}
				})
			}
			wg.Wait()
			entries, err := s.Reflog("refs/heads/race")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != successes+1 {
				t.Errorf("%d reflog entries for %d successful updates", len(entries), successes)
			}
			for i := 0; i+1 < len(entries); i++ {
				if entries[i].Old != entries[i+1].New {
					t.Errorf("reflog entry %d starts at %s, previous ended at %s", i, entries[i].Old, entries[i+1].New)
				}
			}
			if got := r.git("rev-parse", "race"); got != entries[0].New.String() {
				t.Errorf("race = %s, newest reflog entry says %s", got, entries[0].New)
			}
			verifyRefs(t, r)
		})
	}
}
