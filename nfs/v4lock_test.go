package nfs

import (
	"cmp"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// v4heldOn lists the ranges held of file, by start then owner.
func v4heldOn(st *v4State, file uint64) []v4Conflict {
	st.mu.Lock()
	defer st.mu.Unlock()
	return v4tableOn(&st.locks, file)
}

// v4tableOn lists the ranges a table holds of file, by start then owner.
func v4tableOn(l *v4Locks, file uint64) []v4Conflict {
	var held []v4Conflict
	for owner, ranges := range l.files[file] {
		for _, r := range ranges {
			held = append(held, v4Conflict{r, owner})
		}
	}
	slices.SortFunc(held, func(a, b v4Conflict) int {
		if a.start != b.start {
			return cmp.Compare(a.start, b.start)
		}
		if lessOwner4(a.owner, b.owner) {
			return -1
		}
		return 1
	})
	return held
}

var (
	v4ownerA = v4Owner{client: 1, name: "a"}
	v4ownerB = v4Owner{client: 2, name: "b"}
)

// v4mustLock locks on a table and fails the test on anything but success.
func v4mustLock(t *testing.T, l *v4Locks, owner v4Owner, file uint64, start, end uint64, excl bool) {
	t.Helper()
	if c, s := l.lock(owner, file, v4Range{start, end, excl}); s != stOK {
		t.Fatalf("lock [%d, %d) by %v: %+v, status %d", start, end, owner, c, s)
	}
}

// v4assertHeld compares the ranges held of file.
func v4assertHeld(t *testing.T, l *v4Locks, file uint64, want ...v4Conflict) {
	t.Helper()
	if got := v4tableOn(l, file); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("locks of %d = %+v, want %+v", file, got, want)
	}
}

func TestV4LocksConflictBetweenOwners(t *testing.T) {
	l := newLocks4(v4MaxLocksPerFile, v4MaxLocksPerClient)
	v4mustLock(t, &l, v4ownerA, 1, 0, 100, true)
	c, s := l.lock(v4ownerB, 1, v4Range{50, 150, true})
	if want := (v4Conflict{v4Range{0, 100, true}, v4ownerA}); s != stDenied || *c != want {
		t.Fatalf("overlapping write lock = %+v, %d; want %+v", c, s, want)
	}
	if _, s := l.lock(v4ownerB, 1, v4Range{0, 10, false}); s != stDenied {
		t.Fatal("a read lock over another owner's write lock must conflict")
	}
	v4mustLock(t, &l, v4ownerB, 1, 200, 300, true)
	// Read locks coexist.
	v4mustLock(t, &l, v4ownerA, 2, 0, 10, false)
	v4mustLock(t, &l, v4ownerB, 2, 0, 10, false)
	// Refused requests granted nothing.
	v4assertHeld(t, &l, 1, v4Conflict{v4Range{0, 100, true}, v4ownerA}, v4Conflict{v4Range{200, 300, true}, v4ownerB})
}

func TestV4LocksReportLowestConflict(t *testing.T) {
	l := newLocks4(v4MaxLocksPerFile, v4MaxLocksPerClient)
	v4mustLock(t, &l, v4ownerA, 1, 40, 50, true)
	v4mustLock(t, &l, v4Owner{client: 3, name: "c"}, 1, 10, 20, false)
	v4mustLock(t, &l, v4ownerA, 1, 10, 20, false)
	for range 20 {
		if c := l.conflict(v4ownerB, 1, v4Range{0, 100, true}); c == nil || c.start != 10 || c.owner != v4ownerA {
			t.Fatalf("conflict %+v, want the lowest range and owner every time", c)
		}
	}
}

func TestV4LocksSameOwnerReplacesAndUnlockSplits(t *testing.T) {
	l := newLocks4(v4MaxLocksPerFile, v4MaxLocksPerClient)
	v4mustLock(t, &l, v4ownerA, 1, 0, 100, true)
	// Downgrading the middle splits the write lock around a read lock.
	v4mustLock(t, &l, v4ownerA, 1, 40, 60, false)
	v4assertHeld(t, &l, 1,
		v4Conflict{v4Range{0, 40, true}, v4ownerA},
		v4Conflict{v4Range{40, 60, false}, v4ownerA},
		v4Conflict{v4Range{60, 100, true}, v4ownerA})
	l.unlock(v4ownerA, 1, v4Range{20, 80, true})
	v4assertHeld(t, &l, 1, v4Conflict{v4Range{0, 20, true}, v4ownerA}, v4Conflict{v4Range{80, 100, true}, v4ownerA})
	// Adjacent ranges of the same kind merge.
	v4mustLock(t, &l, v4ownerA, 1, 20, 80, true)
	v4assertHeld(t, &l, 1, v4Conflict{v4Range{0, 100, true}, v4ownerA})
	// Unlocking bytes the owner does not hold changes nothing.
	l.unlock(v4ownerB, 1, v4Range{0, 10, true})
	v4assertHeld(t, &l, 1, v4Conflict{v4Range{0, 100, true}, v4ownerA})
}

func TestV4LocksReleaseOwnerAndClient(t *testing.T) {
	l := newLocks4(v4MaxLocksPerFile, v4MaxLocksPerClient)
	sameClient := v4Owner{client: v4ownerA.client, name: "a2"}
	v4mustLock(t, &l, v4ownerA, 1, 0, 10, true)
	v4mustLock(t, &l, v4ownerA, 2, 0, 10, true)
	v4mustLock(t, &l, sameClient, 3, 0, 10, true)
	v4mustLock(t, &l, v4ownerB, 1, 20, 30, true)

	l.releaseOwner(v4ownerA)
	v4assertHeld(t, &l, 1, v4Conflict{v4Range{20, 30, true}, v4ownerB})
	v4assertHeld(t, &l, 2)
	v4assertHeld(t, &l, 3, v4Conflict{v4Range{0, 10, true}, sameClient})

	l.releaseClient(v4ownerA.client)
	v4assertHeld(t, &l, 3)
	v4assertHeld(t, &l, 1, v4Conflict{v4Range{20, 30, true}, v4ownerB})
	if n := l.perClient[v4ownerA.client]; n != 0 {
		t.Fatalf("client count after release = %d, want 0", n)
	}
	if len(l.owned) != 1 || len(l.files) != 1 {
		t.Fatalf("indexes kept released owners: %v %v", l.owned, l.files)
	}
}

func TestV4LockLimitsAreAtomic(t *testing.T) {
	l := newLocks4(2, v4MaxLocksPerClient)
	v4mustLock(t, &l, v4ownerA, 1, 0, 10, true)
	v4mustLock(t, &l, v4ownerA, 1, 20, 30, true)
	// A third separate range exceeds the per-file cap and changes nothing.
	if _, s := l.lock(v4ownerA, 1, v4Range{40, 50, true}); s != stResource {
		t.Fatalf("lock over the file cap: status %d, want RESOURCE", s)
	}
	v4assertHeld(t, &l, 1, v4Conflict{v4Range{0, 10, true}, v4ownerA}, v4Conflict{v4Range{20, 30, true}, v4ownerA})
	// Extending a held range adds none, so it stays under the cap.
	v4mustLock(t, &l, v4ownerA, 1, 10, 20, true)
	v4assertHeld(t, &l, 1, v4Conflict{v4Range{0, 30, true}, v4ownerA})

	c := newLocks4(v4MaxLocksPerFile, 1)
	v4mustLock(t, &c, v4ownerA, 1, 0, 10, true)
	if _, s := c.lock(v4ownerA, 2, v4Range{0, 10, true}); s != stResource {
		t.Fatalf("lock over the client cap: status %d, want RESOURCE", s)
	}
	c.releaseOwner(v4ownerA)
	v4mustLock(t, &c, v4ownerA, 2, 0, 10, true)
}

// TestV4LocksConcurrentOwners locks and unlocks through the state from many
// goroutines.
func TestV4LocksConcurrentOwners(t *testing.T) {
	st := v4newState(t, nil)
	var wg sync.WaitGroup
	for i := range 16 {
		owner := v4Owner{client: v4testClient(t, st, fmt.Sprint(i)), name: "o"}
		open := v4testOpen(t, st, owner, 1)
		start := uint64(i * 10)
		wg.Go(func() {
			id, _, s := st.lock(open, owner, 1, v4Range{start, start + 10, true})
			for range 100 {
				if s != stOK {
					t.Errorf("owner %d: lock status %d", i, s)
					return
				}
				if id, s = st.unlockRange(id, 1, v4Range{start, start + 10, true}); s != stOK {
					t.Errorf("owner %d: unlock status %d", i, s)
					return
				}
				id, _, s = st.relock(id, 1, v4Range{start, start + 10, true})
			}
			if _, s := st.unlockRange(id, 1, v4Range{start, start + 10, true}); s != stOK {
				t.Errorf("owner %d: final unlock status %d", i, s)
			}
		})
	}
	wg.Wait()
	if held := v4heldOn(st, 1); len(held) != 0 {
		t.Fatalf("held %+v", held)
	}
}

// TestV4LockThroughCompound locks over the wire and checks the LOCK4denied
// another client's request gets, then LOCKT, LOCKU and RELEASE_LOCKOWNER.
func TestV4LockThroughCompound(t *testing.T) {
	fs := newTestFS(t)
	srv, addr := v4serve(t, fs, nil)
	lock := func(name string) (v4Owner, *v4res, *v4conn) {
		c := v4dial(t, addr)
		c.setup(srv, name, nil)
		o := c.mustOpen("o", "f", share4AccessAll, true)
		owner := c.owner(name)
		r := c.run(v4Root(), v4Look("f"), v4Lock(v4lockArgs{typ: lock4Write, length: 100, fresh: true, open: o.id, owner: owner}))
		r.skip(op4PutRootFH, op4Lookup)
		return owner, r, c
	}
	alice, r, ac := lock("alice")
	locked := getStateID4(r.next(op4Lock, stOK))
	if locked.seqid != 1 {
		t.Fatalf("lock stateid %+v, want a new state at seqid 1", locked)
	}
	r.end()

	bob, r, bc := lock("bob")
	if r.status != stDenied {
		t.Fatalf("conflicting LOCK: status %d, want DENIED", r.status)
	}
	d := r.next(op4Lock, stDenied)
	if off, length, typ, owner := d.u64(), d.u64(), d.u32(), getOwner4(d); off != 0 || length != 100 || typ != lock4Write || owner != alice {
		t.Fatalf("LOCK4denied %d %d %d %+v", off, length, typ, owner)
	}
	r.end()

	r = bc.run(v4Root(), v4Look("f"), v4LockT(lock4Read, 50, length4EOF, bob), v4GetFH())
	if r.status != stDenied || r.n != 3 {
		t.Fatalf("LOCKT: status %d, %d results", r.status, r.n)
	}
	r.skip(op4PutRootFH, op4Lookup)
	d = r.next(op4LockT, stDenied)
	if off, length := d.u64(), d.u64(); off != 0 || length != 100 {
		t.Fatalf("LOCKT denied %d+%d", off, length)
	}
	d.u32()
	getOwner4(d)
	r.end()

	r = ac.run(v4Root(), v4Look("f"), v4LockU(lock4Write, locked, 0, 50),
		v4Lock(v4lockArgs{typ: lock4Read, offset: 200, length: 1, lockID: locked}))
	r.skip(op4PutRootFH, op4Lookup)
	if id := getStateID4(r.next(op4LockU, stOK)); id.seqid != 2 {
		t.Fatalf("LOCKU stateid %+v", id)
	}
	if id := getStateID4(r.next(op4Lock, stOK)); id.seqid != 3 {
		t.Fatalf("LOCK by stateid %+v", id)
	}
	r.end()
	if s := bc.run(v4Root(), v4Look("f"), v4LockT(lock4Write, 0, 50, bob)).status; s != stOK {
		t.Fatalf("LOCKT of the unlocked range: status %d", s)
	}
	if s := ac.run(func(e *encoder) { e.u32(op4ReleaseLockOwner); e.u64(alice.client); e.str(alice.name) }).status; s != stOK {
		t.Fatalf("RELEASE_LOCKOWNER: status %d", s)
	}
	if s := bc.run(v4Root(), v4Look("f"), v4LockT(lock4Write, 0, length4EOF, bob)).status; s != stOK {
		t.Fatalf("LOCKT after RELEASE_LOCKOWNER: status %d", s)
	}
	for _, tc := range []struct {
		what string
		args v4lockArgs
		want status
	}{
		{"zero length", v4lockArgs{typ: lock4Write, lockID: locked}, stInval},
		{"released lock stateid", v4lockArgs{typ: lock4Write, length: 1, lockID: locked}, stBadStateID},
		{"reclaim during grace", v4lockArgs{typ: lock4Write, length: 1, lockID: locked, reclaim: true}, stBadStateID},
	} {
		if s := ac.run(v4Root(), v4Look("f"), v4Lock(tc.args)).status; s != tc.want {
			t.Errorf("%s: status %d, want %d", tc.what, s, tc.want)
		}
	}
}
