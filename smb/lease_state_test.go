package smb

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// leaseStateFixture observes real unsolicited SMB packets while driving the
// registry directly, keeping cache-transition races independent of CREATE I/O.
type leaseStateFixture struct {
	t      *testing.T
	c      *conn
	sess   *session
	tree   *tree
	wire   *wireClient
	nextID uint64
}

func newLeaseStateFixture(t *testing.T) *leaseStateFixture {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	if err := server.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	c := &conn{srv: &Server{FS: &testBackend{}}, nc: server, dialect: 0x311, clientGUID: [16]byte{1}}
	sess := &session{id: 1, conn: c}
	tr := &tree{id: 1, sess: sess, files: make(map[fileID]*open)}
	c.sessions = map[uint64]*session{sess.id: sess}
	sess.trees = map[uint32]*tree{tr.id: tr}
	t.Cleanup(c.shutdown)
	return &leaseStateFixture{t: t, c: c, sess: sess, tree: tr, wire: &wireClient{t: t, conn: client}}
}

func (f *leaseStateFixture) open(p string, a createArgs) (*open, uint16, uint32) {
	f.t.Helper()
	f.nextID++
	o := &open{id: fileID{f.nextID, f.nextID}, tree: f.tree, path: p}
	g := &f.c.srv.st.opens
	epoch, flags := g.grantCaching(f.c, o, a)
	g.add(o)
	f.tree.files[o.id] = o
	o.closeFn = func() {
		g.dropCaching(o)
		g.remove(o)
	}
	return o, epoch, flags
}

func leaseStateArgs(version int, key byte, state uint32, epoch uint16) createArgs {
	return createArgs{oplock: 255, leaseVersion: version, leaseKey: [16]byte{key}, leaseState: state, leaseEpoch: epoch}
}

func (f *leaseStateFixture) breaks(count int, start func() []*oplockGrant) ([]*oplockGrant, [][]byte) {
	f.t.Helper()
	result := make(chan []*oplockGrant, 1)
	go func() { result <- start() }()
	var bodies [][]byte
	for range count {
		raw, err := f.wire.readFrame()
		if err != nil {
			f.t.Fatal(err)
		}
		r, err := decodeWireReply(raw)
		if err != nil {
			f.t.Fatal(err)
		}
		if r.command != cmdOplockBreak || binary.LittleEndian.Uint64(raw[24:]) != ^uint64(0) || r.session != 0 || r.tree != 0 {
			f.t.Fatalf("invalid server-initiated break envelope: %x", raw)
		}
		bodies = append(bodies, r.body)
	}
	select {
	case pending := <-result:
		return pending, bodies
	case <-time.After(5 * time.Second):
		f.t.Fatal("break did not finish sending; possible duplicate notification")
		return nil, nil
	}
}

func (f *leaseStateFixture) ack(c *conn, sess *session, key [16]byte, state uint32) uint32 {
	f.t.Helper()
	body := make([]byte, 36)
	binary.LittleEndian.PutUint16(body, 36)
	copy(body[8:], key[:])
	binary.LittleEndian.PutUint32(body[24:], state)
	out, status := handleOplockBreak(&request{c: c, sess: sess, tree: f.tree, body: body})
	if status == statusSuccess && (len(out) != 36 || binary.LittleEndian.Uint32(out[24:]) != state) {
		f.t.Fatalf("invalid lease ACK response: %x", out)
	}
	return status
}

func assertLeaseCache(t *testing.T, o *open, state uint32) {
	t.Helper()
	o.mu.Lock()
	got := o.leaseState
	o.mu.Unlock()
	if got != state {
		t.Fatalf("open's caching state = %#x, want %#x", got, state)
	}
}

func assertLeaseBreakPending(t *testing.T, pending []*oplockGrant, want bool) {
	t.Helper()
	if len(pending) != 1 {
		t.Fatalf("break groups = %d, want 1", len(pending))
	}
	select {
	case <-pending[0].done:
		if want {
			t.Fatal("break waiter released before ACK or final close")
		}
	default:
		if !want {
			t.Fatal("break waiter not released")
		}
	}
}

func TestSMBLeaseEpochPromotionAndBreak(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		version                   int
		initial, promoted, broken uint16
	}{
		{"v1-on-SMB3", 32, 0, 0, 0},
		{"v2-wraparound", 52, 65535, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLeaseStateFixture(t)
			a := leaseStateArgs(tc.version, 1, 1, 65534)
			first, epoch, _ := f.open("file", a)
			if epoch != tc.initial {
				t.Fatalf("initial epoch = %d, want %d", epoch, tc.initial)
			}
			a.leaseState, a.leaseEpoch = 7, 1234
			second, epoch, _ := f.open("file", a)
			if epoch != tc.promoted {
				t.Fatalf("promotion epoch = %d, want %d", epoch, tc.promoted)
			}
			assertLeaseCache(t, first, 3)
			assertLeaseCache(t, second, 3)
			// A reopen requesting less caching cannot downgrade the shared lease
			// or import its client epoch into the existing server lease.
			a.leaseState, a.leaseEpoch = 5, 60000
			third, epoch, _ := f.open("file", a)
			if epoch != tc.promoted {
				t.Fatalf("non-promotion changed epoch to %d", epoch)
			}
			assertLeaseCache(t, third, 3)
			g := &f.c.srv.st.opens
			pending, packets := f.breaks(1, func() []*oplockGrant { return g.breakConflicting(f.c, "file", [16]byte{9}, true) })
			body := packets[0]
			if len(body) != 44 || binary.LittleEndian.Uint16(body[2:]) != tc.broken || binary.LittleEndian.Uint32(body[4:]) != 1 || binary.LittleEndian.Uint32(body[24:]) != 3 || binary.LittleEndian.Uint32(body[28:]) != 0 {
				t.Fatalf("incorrect promoted lease break: %x", body)
			}
			if got := f.ack(f.c, f.sess, a.leaseKey, 0); got != statusSuccess {
				t.Fatalf("ACK status = %#x", got)
			}
			assertLeaseBreakPending(t, pending, false)
			assertLeaseCache(t, first, 0)
			assertLeaseCache(t, second, 0)
			assertLeaseCache(t, third, 0)
		})
	}
}

func TestSMBLeaseGroupedBreakRejectsInvalidACK(t *testing.T) {
	f := newLeaseStateFixture(t)
	a := leaseStateArgs(52, 1, 7, 0)
	first, _, _ := f.open("file", a)
	second, _, _ := f.open("file", a)
	otherSession := &session{id: 2, conn: f.c}
	second.tree = &tree{id: 2, sess: otherSession}
	g := &f.c.srv.st.opens
	pending, _ := f.breaks(1, func() []*oplockGrant { return g.breakConflicting(f.c, "file", [16]byte{}, false) })
	wrongClient := &conn{srv: f.c.srv, clientGUID: [16]byte{2}}
	if got := f.ack(wrongClient, f.sess, a.leaseKey, 0); got != statusObjectNameNotFound {
		t.Fatalf("other client's ACK status = %#x", got)
	}
	assertLeaseBreakPending(t, pending, true)
	if got := f.ack(f.c, otherSession, a.leaseKey, 1); got != statusRequestNotAccepted {
		t.Fatalf("ACK retaining revoked READ caching status = %#x", got)
	}
	assertLeaseBreakPending(t, pending, true)
	assertLeaseCache(t, first, 3)
	assertLeaseCache(t, second, 3)
	if got := f.ack(f.c, otherSession, a.leaseKey, 0); got != statusSuccess {
		t.Fatalf("same client, different session ACK status = %#x", got)
	}
	assertLeaseBreakPending(t, pending, false)
	assertLeaseCache(t, first, 0)
	assertLeaseCache(t, second, 0)
	if got := f.ack(f.c, f.sess, a.leaseKey, 0); got != statusUnsuccessful {
		t.Fatalf("duplicate ACK status = %#x", got)
	}
}

func TestSMBLeaseReopenCannotBypassPendingBreak(t *testing.T) {
	f := newLeaseStateFixture(t)
	a := leaseStateArgs(52, 1, 3, 10)
	first, epoch, _ := f.open("file", a)
	g := &f.c.srv.st.opens
	pending, _ := f.breaks(1, func() []*oplockGrant { return g.breakConflicting(f.c, "file", [16]byte{2}, true) })
	a.leaseState, a.leaseEpoch = 7, 50000
	reopened, reopenedEpoch, flags := f.open("file", a)
	if reopenedEpoch != epoch+1 || flags&2 == 0 {
		t.Fatalf("pending reopen epoch=%d flags=%#x, want epoch=%d BREAK_IN_PROGRESS", reopenedEpoch, flags, epoch+1)
	}
	assertLeaseCache(t, reopened, 3)
	again, _ := f.breaks(0, func() []*oplockGrant { return g.breakConflicting(f.c, "file", [16]byte{3}, true) })
	assertLeaseBreakPending(t, pending, true)
	assertLeaseBreakPending(t, again, true)
	if got := f.ack(f.c, f.sess, a.leaseKey, 0); got != statusSuccess {
		t.Fatalf("ACK status = %#x", got)
	}
	assertLeaseCache(t, first, 0)
	assertLeaseCache(t, reopened, 0)
	if !g.waitBreaks(pending, nil) || !g.waitBreaks(again, nil) {
		t.Fatal("ACK did not release all conflicting opens")
	}
}

func TestSMBLeaseReadOnlyBreakNeedsNoACK(t *testing.T) {
	f := newLeaseStateFixture(t)
	a := leaseStateArgs(52, 1, 5, 0)
	o, _, _ := f.open("file", a)
	assertLeaseCache(t, o, 1)
	g := &f.c.srv.st.opens
	pending, packets := f.breaks(1, func() []*oplockGrant { return g.breakDescendants("file") })
	if binary.LittleEndian.Uint32(packets[0][4:]) != 0 || binary.LittleEndian.Uint32(packets[0][28:]) != 0 {
		t.Fatalf("READ-only break incorrectly requires ACK: %x", packets[0])
	}
	assertLeaseCache(t, o, 0)
	assertLeaseBreakPending(t, pending, false)
	if !g.waitBreaks(pending, nil) {
		t.Fatal("READ-only break did not complete")
	}
}

func TestSMBLeaseLastCloseAndDisconnectReleaseWaiters(t *testing.T) {
	f := newLeaseStateFixture(t)
	a := leaseStateArgs(52, 1, 3, 0)
	first, _, _ := f.open("file", a)
	second, _, _ := f.open("file", a)
	g := &f.c.srv.st.opens
	pending, _ := f.breaks(1, func() []*oplockGrant { return g.breakDescendants("file") })
	closeImplicit(first)
	assertLeaseBreakPending(t, pending, true)
	assertLeaseCache(t, second, 3)
	f.c.shutdown()
	assertLeaseBreakPending(t, pending, false)
	if !g.waitBreaks(pending, nil) {
		t.Fatal("disconnect did not release a conflicting open")
	}
}

func TestSMBLeaseDisconnectedTransportRetriesGroupedBreak(t *testing.T) {
	first := newLeaseStateFixture(t)
	second := newLeaseStateFixture(t)
	second.c.srv = first.c.srv
	a := leaseStateArgs(52, 1, 3, 0)
	first.open("file", a)
	secondOpen, _, _ := second.open("file", a)
	g := &first.c.srv.st.opens
	pending, original := first.breaks(1, func() []*oplockGrant { return g.breakDescendants("file") })
	_, retry := second.breaks(1, func() []*oplockGrant {
		first.c.shutdown()
		return pending
	})
	if binary.LittleEndian.Uint16(original[0][2:]) != binary.LittleEndian.Uint16(retry[0][2:]) {
		t.Fatal("transport retry incorrectly started a new lease epoch")
	}
	assertLeaseBreakPending(t, pending, true)
	// Disconnecting one transport cannot revoke the surviving client's lease
	// before its cached-handle transition is acknowledged.
	assertLeaseCache(t, secondOpen, 3)
	if got := second.ack(second.c, second.sess, a.leaseKey, 0); got != statusSuccess {
		t.Fatalf("surviving transport ACK status = %#x", got)
	}
	assertLeaseBreakPending(t, pending, false)
	assertLeaseCache(t, secondOpen, 0)
}

func TestSMBLeaseCompletedWaiterDoesNotFollowLaterBreak(t *testing.T) {
	f := newLeaseStateFixture(t)
	a := leaseStateArgs(52, 1, 3, 0)
	f.open("file", a)
	g := &f.c.srv.st.opens
	first, _ := f.breaks(1, func() []*oplockGrant { return g.breakDescendants("file") })
	if got := f.ack(f.c, f.sess, a.leaseKey, 0); got != statusSuccess {
		t.Fatalf("ACK status = %#x", got)
	}
	f.open("file", a)
	second, _ := f.breaks(1, func() []*oplockGrant { return g.breakDescendants("file") })
	finished := make(chan bool, 1)
	go func() { finished <- g.waitBreaks(first, nil) }()
	select {
	case ok := <-finished:
		if !ok {
			t.Fatal("completed break was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completed waiter followed a newer break")
	}
	assertLeaseBreakPending(t, second, true)
	if got := f.ack(f.c, f.sess, a.leaseKey, 0); got != statusSuccess {
		t.Fatalf("second ACK status = %#x", got)
	}
}

func TestSMBLeaseDescendantBreakIncludesOwnKeyNotSibling(t *testing.T) {
	f := newLeaseStateFixture(t)
	var children []*open
	for i, name := range []string{"parent", "parent/a", "parent/deep/b"} {
		o, _, _ := f.open(name, leaseStateArgs(52, byte(i+1), 3, 0))
		children = append(children, o)
	}
	sibling, _, _ := f.open("parentish/a", leaseStateArgs(52, 4, 3, 0))
	g := &f.c.srv.st.opens
	pending, packets := f.breaks(3, func() []*oplockGrant { return g.breakDescendants("parent") })
	if len(pending) != 3 {
		t.Fatalf("descendant break groups = %d, want 3", len(pending))
	}
	for _, packet := range packets {
		var key [16]byte
		copy(key[:], packet[8:24])
		if key[0] < 1 || key[0] > 3 {
			t.Fatalf("break reached sibling lease: %x", key)
		}
		if got := f.ack(f.c, f.sess, key, 0); got != statusSuccess {
			t.Fatalf("descendant ACK status = %#x", got)
		}
	}
	for _, o := range children {
		assertLeaseCache(t, o, 0)
	}
	assertLeaseCache(t, sibling, 3)
	if !g.waitBreaks(pending, nil) {
		t.Fatal("descendant ACKs did not release mutation")
	}
}

func TestSMBLeaseCancelledWaitStillExpiresBreak(t *testing.T) {
	f := newLeaseStateFixture(t)
	a := leaseStateArgs(52, 1, 3, 0)
	o, _, _ := f.open("file", a)
	g := &f.c.srv.st.opens
	pending, _ := f.breaks(1, func() []*oplockGrant { return g.breakDescendants("file") })
	cancelled := make(chan struct{})
	close(cancelled)
	if g.waitBreaks(pending, cancelled) {
		t.Fatal("pending mutation ignored cancellation")
	}
	assertLeaseBreakPending(t, pending, true)
	// Expire the real grant-owned deadline without waiting 35 seconds. This
	// must still revoke the lease after its initiating operation cancelled.
	g.mu.Lock()
	timer := g.grants[o].timer
	if timer != nil {
		timer.Reset(0)
	}
	g.mu.Unlock()
	if timer == nil {
		t.Fatal("cancelled mutation abandoned the lease break deadline")
	}
	select {
	case <-pending[0].done:
	case <-time.After(5 * time.Second):
		t.Fatal("lease break did not expire after its waiter cancelled")
	}
	assertLeaseCache(t, o, 0)
	if !g.waitBreaks(pending, nil) {
		t.Fatal("timed-out break did not release subsequent mutations")
	}
	if got := f.ack(f.c, f.sess, a.leaseKey, 0); got != statusUnsuccessful {
		t.Fatalf("late ACK after timeout status = %#x", got)
	}
}

func TestSMBWriteCachingRequestsAreDowngraded(t *testing.T) {
	for _, tc := range []struct {
		name string
		args createArgs
	}{
		{"lease-RWH", leaseStateArgs(52, 1, 7, 0)},
		{"exclusive-oplock", createArgs{oplock: 8}},
		{"batch-oplock", createArgs{oplock: 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLeaseStateFixture(t)
			o, _, _ := f.open("file", tc.args)
			if tc.args.oplock == 255 {
				assertLeaseCache(t, o, 3)
			} else if o.oplock != 1 {
				t.Fatalf("write-caching oplock was not downgraded to level II: %#x", o.oplock)
			}
			pending, packets := f.breaks(1, func() []*oplockGrant { return f.c.srv.st.opens.breakDescendants("file") })
			if tc.args.oplock == 255 {
				if got := f.ack(f.c, f.sess, tc.args.leaseKey, 0); got != statusSuccess {
					t.Fatalf("HANDLE break ACK status = %#x", got)
				}
			} else if len(packets[0]) != 24 || packets[0][2] != 0 || o.oplock != 0 {
				t.Fatalf("downgraded classic oplock did not break to NONE: %x", packets[0])
			}
			assertLeaseBreakPending(t, pending, false)
		})
	}
}

type uncacheableLeaseBackend struct{ Backend }

func (uncacheableLeaseBackend) Cacheable(string) bool { return false }

func TestSMBUncacheablePathReceivesNoCaching(t *testing.T) {
	f := newLeaseStateFixture(t)
	f.c.srv.FS = uncacheableLeaseBackend{f.c.srv.FS}
	lease, _, _ := f.open("untracked", leaseStateArgs(52, 1, 7, 0))
	assertLeaseCache(t, lease, 0)
	classic, _, _ := f.open("another-untracked", createArgs{oplock: 9})
	if classic.oplock != 0 {
		t.Fatalf("untracked path received oplock %#x", classic.oplock)
	}
	pending, _ := f.breaks(0, func() []*oplockGrant { return f.c.srv.st.opens.breakDescendants(".") })
	if len(pending) != 0 {
		t.Fatalf("untracked paths retained %d cache grants", len(pending))
	}
}
