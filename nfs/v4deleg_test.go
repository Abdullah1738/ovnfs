package nfs

import (
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// v4delegServer serves a test filesystem whose files are all cacheable,
// with tune adjusting the server, and the policy, before it starts.
func v4delegServer(t *testing.T, tune func(*Server)) (*testFS, *Server, string) {
	fs := newTestFS(t)
	fs.cacheable = func(string) bool { return true }
	srv, addr := v4serve(t, fs, tune)
	return fs, srv, addr
}

// v4policy sets the test filesystem's Backend.Cacheable from tune, before
// the server starts.
func v4policy(s *Server, cacheable func(fs *testFS, p string) bool) {
	fs := s.FS.(*testFS)
	fs.cacheable = func(p string) bool { return cacheable(fs, p) }
}

// v4holder sets up a client with a callback service.
func v4holder(t *testing.T, srv *Server, addr, name string) (*v4conn, *v4FakeCB) {
	cb := newV4FakeCB(t)
	c := v4dial(t, addr)
	c.setup(srv, name, cb)
	return c, cb
}

// mustDelegate opens name for reading and returns the read delegation that
// must come with it.
func (c *v4conn) mustDelegate(owner, name string) v4StateID {
	c.t.Helper()
	o := c.mustOpen(owner, name, share4Read, false)
	if o.deleg == nil {
		c.t.Fatalf("read OPEN of %s: no delegation", name)
	}
	return *o.deleg
}

// delegReturn returns a delegation.
func (c *v4conn) delegReturn(id v4StateID) status {
	c.t.Helper()
	return c.run(v4Root(), v4DelegReturn(id)).status
}

// readWith reads name with stateid id.
func (c *v4conn) readWith(name string, id v4StateID) (status, string) {
	c.t.Helper()
	r := c.run(v4Root(), v4Look(name), v4Read(id, 0, 100))
	if r.status != stOK {
		return r.status, ""
	}
	r.skip(op4PutRootFH, op4Lookup)
	d := r.next(op4Read, stOK)
	d.bool()
	return stOK, string(d.opaque(100))
}

func TestV4ReadOpenGetsDelegation(t *testing.T) {
	fs, srv, addr := v4delegServer(t, nil)
	c, cb := v4holder(t, srv, addr, "c")
	v4write(t, fs, "f", "data")

	id := c.mustDelegate("reader", "f")
	if s, data := c.readWith("f", id); s != stOK || data != "data" {
		t.Fatalf("READ with the delegation stateid: status %d, data %q", s, data)
	}
	if s := c.run(v4Root(), v4Look("f"), v4Write(id, 0, "x")).status; s != stOpenMode {
		t.Fatalf("WRITE with a read delegation stateid: status %d, want OPENMODE", s)
	}
	if s := c.run(v4Root(), v4Look("f"), v4SetAttr(id, v4Size(0))).status; s != stOpenMode {
		t.Fatalf("truncate with a read delegation stateid: status %d, want OPENMODE", s)
	}
	if s := c.run(v4Root(), v4Look("f"), v4SetAttr(id, v4Mode(0o600))).status; s != stOK {
		t.Fatalf("SETATTR mode with a read delegation stateid: status %d", s)
	}
	// Opening the file again while holding the delegation adds none.
	if o := c.mustOpen("other", "f", share4Read, false); o.deleg != nil {
		t.Fatal("second delegation granted of the same file")
	}
	if s := c.delegReturn(id); s != stOK {
		t.Fatalf("DELEGRETURN: status %d", s)
	}
	if s := c.delegReturn(id); s != stBadStateID {
		t.Fatalf("second DELEGRETURN: status %d, want BAD_STATEID", s)
	}
	if s, _ := c.readWith("f", id); s != stBadStateID {
		t.Fatalf("READ with a returned delegation: status %d, want BAD_STATEID", s)
	}
	if s := c.run(v4DelegReturn(id)).status; s != stNoFileHandle {
		t.Fatalf("DELEGRETURN without a filehandle: status %d", s)
	}
	cb.expectNoRecall(t)
	if n := srv.v4.delegs.Load(); n != 0 {
		t.Fatalf("%d delegations outstanding", n)
	}
}

// TestV4RecallDuringGrant: Backend.Cacheable is consulted without the
// server's locks held, and a change to the file meanwhile, reported with
// Recall or made by another client's WRITE, refuses the grant; a change to
// another file does not.
func TestV4RecallDuringGrant(t *testing.T) {
	var mu sync.Mutex
	var writer *v4conn
	race := true
	fs, srv, addr := v4delegServer(t, func(s *Server) {
		v4policy(s, func(fs *testFS, p string) bool {
			mu.Lock()
			defer mu.Unlock()
			if !race {
				return true
			}
			switch p {
			case "changing":
				s.Recall(fs.FileID("changing"))
			case "written":
				if st := writer.run(v4Root(), v4Look("written"), v4Write(stateid4Anon, 0, "x")).status; st != stOK {
					t.Errorf("WRITE during the grant: status %d", st)
				}
			default:
				s.Recall(fs.FileID("elsewhere"))
			}
			return true
		})
	})
	for _, name := range []string{"changing", "written", "calm", "elsewhere"} {
		v4write(t, fs, name, "data")
	}
	c, _ := v4holder(t, srv, addr, "c")
	w := v4dial(t, addr)
	w.setup(srv, "w", nil)
	mu.Lock()
	writer = w
	mu.Unlock()

	for _, name := range []string{"changing", "written"} {
		if o := c.mustOpen("o", name, share4Read, false); o.deleg != nil {
			t.Fatalf("delegation of %s granted though it changed while Cacheable was asked", name)
		}
	}
	c.mustDelegate("o", "calm")
	// None of the changes armed the thrash rule.
	mu.Lock()
	race = false
	mu.Unlock()
	c.mustDelegate("o2", "changing")
	c.mustDelegate("o2", "written")
}

func TestV4NoDelegationUnlessSafe(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	fs, srv, addr := v4delegServer(t, func(s *Server) {
		v4policy(s, func(_ *testFS, p string) bool {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, p)
			return p != "private"
		})
	})
	c, _ := v4holder(t, srv, addr, "c")
	noCB := v4dial(t, addr)
	noCB.setup(srv, "nocb", nil)
	w, _ := v4holder(t, srv, addr, "w")
	for _, name := range []string{"f", "private", "written"} {
		v4write(t, fs, name, "data")
	}
	w.mustOpen("w", "written", share4AccessAll, false)

	for _, tc := range []struct {
		what   string
		c      *v4conn
		name   string
		access uint32
	}{
		{"client without a callback path", noCB, "f", share4Read},
		{"open for writing", c, "f", share4AccessAll},
		{"Cacheable refuses", c, "private", share4Read},
		{"another client writes", c, "written", share4Read},
	} {
		if s, o := tc.c.open("o", tc.name, tc.access, false); s != stOK || o.deleg != nil {
			t.Errorf("%s: status %d, delegation %v; want an open without delegation", tc.what, s, o.deleg)
		}
	}
	// Cacheable is asked only when the state would allow a delegation.
	mu.Lock()
	if !slices.Equal(asked, []string{"private"}) {
		t.Errorf("Cacheable asked about %v", asked)
	}
	mu.Unlock()
	// The writer's own read OPEN is no conflict with its write open.
	if o := w.mustOpen("w2", "written", share4Read, false); o.deleg == nil {
		t.Error("writer's read open: no delegation")
	}

	// A client whose callback probe fails gets none.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			nc.Close()
		}
	}()
	dead := v4dial(t, addr)
	a := l.Addr().(*net.TCPAddr)
	r := dead.run(func(e *encoder) {
		e.u32(op4SetClientID)
		e.fixed([]byte("bootverf"))
		e.str("dead")
		e.u32(v4CBProg)
		e.str("tcp")
		e.str("127.0.0.1." + strconv.Itoa(a.Port>>8) + "." + strconv.Itoa(a.Port&0xff))
		e.u32(7)
	})
	d := r.next(op4SetClientID, stOK)
	dead.client = d.u64()
	if s := dead.confirm(dead.client, d.fixed(8)); s != stOK {
		t.Fatalf("confirm: status %d", s)
	}
	v4wait(t, "the probe to fail", func() bool {
		srv.v4.mu.Lock()
		defer srv.v4.mu.Unlock()
		return !srv.v4.clients[dead.client].cbProbing
	})
	if o := dead.mustOpen("o", "f", share4Read, false); o.deleg != nil {
		t.Error("delegation granted over a callback path whose probe failed")
	}
}

// TestV4ClaimDelegateCur: before returning a delegation the macOS client
// re-opens what it opened locally with CLAIM_DELEGATE_CUR.
func TestV4ClaimDelegateCur(t *testing.T) {
	fs, srv, addr := v4delegServer(t, nil)
	c, _ := v4holder(t, srv, addr, "c")
	other, _ := v4holder(t, srv, addr, "o")
	v4write(t, fs, "f", "data")
	id := c.mustDelegate("o", "f")

	claim := func(conn *v4conn, deleg v4StateID) (status, v4opened) {
		a := v4openArgs{owner: conn.owner("local"), access: share4Read, claim: claim4DelegateCur, delegID: deleg, name: "f"}
		r := conn.run(v4Root(), v4Open(a))
		if r.status != stOK {
			return r.status, v4opened{}
		}
		r.skip(op4PutRootFH)
		return stOK, v4getOpened(t, r.next(op4Open, stOK))
	}
	if s, _ := claim(other, id); s != stBadStateID {
		t.Fatalf("CLAIM_DELEGATE_CUR with another client's delegation: status %d, want BAD_STATEID", s)
	}
	s, o := claim(c, id)
	if s != stOK || o.deleg != nil {
		t.Fatalf("CLAIM_DELEGATE_CUR: status %d, delegation %v", s, o.deleg)
	}
	if s := c.delegReturn(id); s != stOK {
		t.Fatalf("DELEGRETURN: status %d", s)
	}
	if s, data := c.readWith("f", o.id); s != stOK || data != "data" {
		t.Fatalf("READ under the claimed open after DELEGRETURN: status %d, data %q", s, data)
	}
	if s, _ := claim(c, id); s != stBadStateID {
		t.Fatalf("CLAIM_DELEGATE_CUR with a returned delegation: status %d, want BAD_STATEID", s)
	}
}

func TestV4RecallSendsCBRecall(t *testing.T) {
	fs, srv, addr := v4delegServer(t, nil)
	c, cb := v4holder(t, srv, addr, "c")
	v4write(t, fs, "f", "data")
	v4write(t, fs, "g", "data")
	id := c.mustDelegate("o", "f")
	file := fs.FileID("f")

	srv.Recall(fs.FileID("g"))
	cb.expectNoRecall(t)
	srv.Recall(file)
	cb.expectRecall(t, id, file)
	srv.Recall(file) // already recalling
	cb.expectNoRecall(t)

	// A file recalled once is delegated again: the recall may have been
	// for a change made before the delegation was granted. A file
	// recalled twice within the quiet period is not, for now.
	if s := c.delegReturn(id); s != stOK {
		t.Fatalf("DELEGRETURN: status %d", s)
	}
	id = c.mustDelegate("o", "f")
	srv.Recall(file)
	cb.expectRecall(t, id, file)
	if s := c.delegReturn(id); s != stOK {
		t.Fatalf("DELEGRETURN: status %d", s)
	}
	if o := c.mustOpen("o2", "f", share4Read, false); o.deleg != nil {
		t.Fatal("delegation granted again of a file recalled twice within the quiet period")
	}
}

// TestV4ThrashRuleEnds: a thrashing file is delegated again once it has gone
// the quiet period without a recall.
func TestV4ThrashRuleEnds(t *testing.T) {
	var clock v4clock
	fs, srv, addr := v4delegServer(t, func(s *Server) { s.v4.now = clock.now })
	c, cb := v4holder(t, srv, addr, "c")
	v4write(t, fs, "f", "data")
	file := fs.FileID("f")
	for range 2 {
		id := c.mustDelegate("o", "f")
		srv.Recall(file)
		cb.expectRecall(t, id, file)
		c.delegReturn(id)
	}
	if o := c.mustOpen("o", "f", share4Read, false); o.deleg != nil {
		t.Fatal("thrashing file delegated")
	}
	clock.advance(v4Quiet + 2*v4SweepEvery)
	c.mustDelegate("o", "f")
}

// TestV4ConflictsWaitForDelegReturn: a request from another client that
// changes a delegated file recalls the delegation and is answered
// NFS4ERR_DELAY until it is returned.
func TestV4ConflictsWaitForDelegReturn(t *testing.T) {
	fs, srv, addr := v4delegServer(t, nil)
	holder, cb := v4holder(t, srv, addr, "holder")
	other := v4dial(t, addr)
	other.setup(srv, "other", nil)

	for i, tc := range []struct {
		what string
		ops  func(name string) []v4op
	}{
		{"OPEN for writing", func(name string) []v4op {
			return []v4op{v4Root(), v4Open(v4openArgs{owner: other.owner(name), access: share4Write, name: name})}
		}},
		{"OPEN creating", func(name string) []v4op {
			return []v4op{v4Root(), v4Open(v4openArgs{owner: other.owner(name), access: share4Read, create: true, name: name})}
		}},
		{"SETATTR", func(name string) []v4op {
			return []v4op{v4Root(), v4Look(name), v4SetAttr(stateid4Anon, v4Size(0))}
		}},
		{"WRITE with a special stateid", func(name string) []v4op {
			return []v4op{v4Root(), v4Look(name), v4Write(stateid4Bypass, 0, "x")}
		}},
		{"REMOVE", func(name string) []v4op {
			return []v4op{v4Root(), v4Remove(name)}
		}},
		{"RENAME source", func(name string) []v4op {
			return []v4op{v4Root(), v4Save(), v4Rename(name, name+".moved")}
		}},
		{"RENAME target", func(name string) []v4op {
			v4write(t, fs, name+".src", "src")
			return []v4op{v4Root(), v4Save(), v4Rename(name+".src", name)}
		}},
		{"LINK", func(name string) []v4op {
			return []v4op{v4Root(), v4Look(name), v4Save(), v4Root(), v4Link(name + ".link")}
		}},
	} {
		name := "f" + strconv.Itoa(i)
		v4write(t, fs, name, "data")
		file := fs.FileID(name)
		id := holder.mustDelegate("reader", name)
		ops := tc.ops(name)
		if s := other.run(ops...).status; s != stDelay {
			t.Fatalf("%s: status %d, want DELAY", tc.what, s)
		}
		cb.expectRecall(t, id, file)
		if s := other.run(ops...).status; s != stDelay {
			t.Fatalf("%s while recalling: status %d, want DELAY", tc.what, s)
		}
		if s := holder.delegReturn(id); s != stOK {
			t.Fatalf("DELEGRETURN: status %d", s)
		}
		if s := other.run(ops...).status; s != stOK {
			t.Fatalf("%s after DELEGRETURN: status %d", tc.what, s)
		}
	}
	cb.expectNoRecall(t)

	// The holder's own changes do not recall its delegation.
	v4write(t, fs, "own", "data")
	holder.mustDelegate("reader", "own")
	holder.mustOpen("writer", "own", share4AccessAll, false)
	if s := holder.run(v4Root(), v4Look("own"), v4Write(stateid4Anon, 0, "x")).status; s != stOK {
		t.Fatalf("holder's WRITE: status %d", s)
	}
	if s := holder.run(v4Root(), v4Remove("own")).status; s != stOK {
		t.Fatalf("holder's REMOVE: status %d", s)
	}
	cb.expectNoRecall(t)
}

// TestNFSv4RecallOfUnseenDelegationIsRetried: a client recalled before it
// processed the OPEN reply granting the delegation answers
// NFS4ERR_BAD_STATEID. The server asks again instead of revoking, which
// would fail the client's later uses of its open state.
func TestNFSv4RecallOfUnseenDelegationIsRetried(t *testing.T) {
	fs, srv, addr := v4delegServer(t, func(s *Server) { s.v4.retryDelay = time.Millisecond })
	holder, cb := v4holder(t, srv, addr, "holder")
	v4write(t, fs, "f", "data")
	file := fs.FileID("f")
	id := holder.mustDelegate("o", "f")
	cb.unknown.Store(2)
	srv.Recall(file)
	for range 3 {
		cb.expectRecall(t, id, file)
	}
	cb.expectNoRecall(t)
	if s, data := holder.readWith("f", id); s != stOK || data != "data" {
		t.Fatalf("READ with the delegation after the retried recall: status %d", s)
	}
	if s := holder.delegReturn(id); s != stOK {
		t.Fatalf("DELEGRETURN after the retried recall: status %d", s)
	}

	// A client answering every CB_RECALL with an error is asked five more
	// times, then left to the recall timeout.
	id = holder.mustDelegate("o2", "f")
	cb.unknown.Store(100)
	srv.Recall(file)
	for range 1 + v4RecallRetries {
		cb.expectRecall(t, id, file)
	}
	cb.expectNoRecall(t)
	if s, _ := holder.readWith("f", id); s != stOK {
		t.Fatalf("READ with a delegation still being recalled: status %d", s)
	}
}

func TestV4UnreturnedDelegationIsRevoked(t *testing.T) {
	var clock v4clock
	fs, srv, addr := v4delegServer(t, func(s *Server) { s.v4.now = clock.now })
	holder, cb := v4holder(t, srv, addr, "holder")
	other := v4dial(t, addr)
	other.setup(srv, "other", nil)
	v4write(t, fs, "f", "data")
	id := holder.mustDelegate("o", "f")

	if s, _ := other.open("o", "f", share4Write, false); s != stDelay {
		t.Fatalf("conflicting OPEN: status %d, want DELAY", s)
	}
	cb.expectRecall(t, id, fs.FileID("f"))
	clock.advance(v4Lease + 2*v4SweepEvery)
	if s, _ := other.open("o", "f", share4Write, false); s != stOK {
		t.Fatalf("conflicting OPEN after the recall timed out: status %d", s)
	}
	if s, _ := holder.readWith("f", id); s != stBadStateID {
		t.Fatalf("READ with the revoked delegation: status %d, want BAD_STATEID", s)
	}
}

func TestV4CallbackFailureMarksPathDown(t *testing.T) {
	var clock v4clock
	fs, srv, addr := v4delegServer(t, func(s *Server) { s.v4.now = clock.now })
	holder, cb := v4holder(t, srv, addr, "holder")
	other := v4dial(t, addr)
	other.setup(srv, "other", nil)
	v4write(t, fs, "f", "data")
	v4write(t, fs, "g", "data")
	id := holder.mustDelegate("o", "f")
	renew := func() status { return holder.run(v4Renew(holder.client)).status }

	cb.fail.Store(true)
	if s := other.run(v4Root(), v4Remove("f")).status; s != stDelay {
		t.Fatalf("REMOVE: status %d, want DELAY", s)
	}
	v4wait(t, "RENEW to report the callback path down", func() bool { return renew() == stCBPathDown })
	if s := other.run(v4Root(), v4Remove("f")).status; s != stOK {
		t.Fatalf("REMOVE after the delegation was revoked: status %d", s)
	}
	if s, _ := holder.readWith("g", id); s != stBadStateID {
		t.Fatalf("READ with the revoked delegation: status %d, want BAD_STATEID", s)
	}
	if o := holder.mustOpen("o", "g", share4Read, false); o.deleg != nil {
		t.Fatal("delegation granted over a callback path that is down")
	}

	// A later probe that gets through brings the path back.
	cb.fail.Store(false)
	clock.advance(v4ProbeEvery)
	v4wait(t, "RENEW to succeed again", func() bool { return renew() == stOK })
	holder.mustDelegate("o2", "g")
}

func TestV4DelegationCapRecallsOldest(t *testing.T) {
	fs, srv, addr := v4delegServer(t, func(s *Server) { s.v4.maxDelegs = 2 })
	c, cb := v4holder(t, srv, addr, "c")
	var ids []v4StateID
	for _, name := range []string{"a", "b", "c"} {
		v4write(t, fs, name, "data")
		ids = append(ids, c.mustDelegate("o", name))
	}
	cb.expectRecall(t, ids[0], fs.FileID("a"))
	cb.expectNoRecall(t)
	c.delegReturn(ids[0])
	// Returned delegations leave the grant order: the next grant recalls b.
	v4write(t, fs, "d", "data")
	c.mustDelegate("o", "d")
	cb.expectRecall(t, ids[1], fs.FileID("b"))
}

func TestV4ExpiredClientLosesDelegations(t *testing.T) {
	var clock v4clock
	fs, srv, addr := v4delegServer(t, func(s *Server) { s.v4.now = clock.now })
	holder, cb := v4holder(t, srv, addr, "holder")
	v4write(t, fs, "f", "data")
	holder.mustDelegate("o", "f")

	clock.advance(v4LeasePeriods*v4Lease + 2*v4SweepEvery)
	other := v4dial(t, addr)
	other.setup(srv, "other", nil)
	if s, _ := other.open("o", "f", share4Write, false); s != stOK {
		t.Fatalf("write OPEN after the holder expired: status %d", s)
	}
	cb.expectNoRecall(t)
	if n := srv.v4.delegs.Load(); n != 0 {
		t.Fatalf("%d delegations outstanding", n)
	}
	if s := holder.run(v4Renew(holder.client)).status; s != stExpired {
		t.Fatalf("RENEW of the expired holder: status %d", s)
	}
}

// TestV4ReplacedClientLosesDelegations: a client that rebooted (a new
// verifier) loses its old id's delegations once the new id is confirmed.
func TestV4ReplacedClientLosesDelegations(t *testing.T) {
	fs, srv, addr := v4delegServer(t, nil)
	holder, cb := v4holder(t, srv, addr, "holder")
	v4write(t, fs, "f", "data")
	holder.mustDelegate("o", "f")
	again := v4dial(t, addr)
	r := again.run(func(e *encoder) {
		e.u32(op4SetClientID)
		e.fixed([]byte("rebooted"))
		e.str("holder")
		e.u32(v4CBProg)
		e.str("")
		e.str("")
		e.u32(7)
	})
	d := r.next(op4SetClientID, stOK)
	id := d.u64()
	if s := again.confirm(id, d.fixed(8)); s != stOK || id == holder.client {
		t.Fatalf("confirm: status %d, id %x (old %x)", s, id, holder.client)
	}
	if n := srv.v4.delegs.Load(); n != 0 {
		t.Fatalf("%d delegations outstanding", n)
	}
	cb.expectNoRecall(t)
}

// TestV4OpenReclaimDuringGrace: after a restart, clients reclaim their opens
// with CLAIM_PREVIOUS and their locks during the grace period, so a file a
// process holds open keeps working. After it, and for delegations, which
// are not reclaimed, the answer is NFS4ERR_NO_GRACE, which sends the client
// to open the file afresh; any other error makes clients give up on the
// open file, failing its reads and writes with EIO.
func TestV4OpenReclaimDuringGrace(t *testing.T) {
	var clock v4clock
	fs := newTestFS(t)
	v4write(t, fs, "f", "data")
	srv, addr := v4serve(t, fs, func(s *Server) { s.v4.now = clock.now })
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	reclaim := func(delegType uint32) *v4res {
		a := v4openArgs{owner: c.owner("o"), access: share4AccessAll, claim: claim4Previous, delegType: delegType}
		return c.run(v4Root(), v4Look("f"), v4Open(a))
	}
	r := reclaim(deleg4None)
	if r.status != stOK {
		t.Fatalf("OPEN CLAIM_PREVIOUS during grace: status %d", r.status)
	}
	r.skip(op4PutRootFH, op4Lookup)
	o := v4getOpened(t, r.next(op4Open, stOK))
	if o.cinfo.before != o.cinfo.after || o.deleg != nil {
		t.Fatalf("reclaimed open %+v", o)
	}
	if s, data := c.readWith("f", o.id); s != stOK || data != "data" {
		t.Fatalf("READ under the reclaimed open: status %d, data %q", s, data)
	}
	if s := reclaim(deleg4Read).status; s != stNoGrace {
		t.Fatalf("delegation reclaim: status %d, want NO_GRACE", s)
	}
	lock := v4Lock(v4lockArgs{typ: lock4Write, reclaim: true, length: 10, fresh: true, open: o.id, owner: c.owner("l")})
	if s := c.run(v4Root(), v4Look("f"), lock).status; s != stOK {
		t.Fatalf("LOCK reclaim during grace: status %d", s)
	}
	if s := c.run(v4Root(), v4Open(v4openArgs{owner: c.owner("o"), access: share4Read, claim: claim4Previous, create: true})).status; s != stInval {
		t.Fatalf("creating reclaim: status %d, want INVAL", s)
	}

	clock.advance(v4Lease + time.Second)
	if s := reclaim(deleg4None).status; s != stNoGrace {
		t.Fatalf("OPEN CLAIM_PREVIOUS after grace: status %d, want NO_GRACE", s)
	}
	if s := c.run(v4Root(), v4Look("f"), lock).status; s != stNoGrace {
		t.Fatalf("LOCK reclaim after grace: status %d, want NO_GRACE", s)
	}
}

func TestV4ParseUniversalAddress(t *testing.T) {
	for _, tc := range []struct {
		netid, uaddr, want string
	}{
		{"tcp", "127.0.0.1.8.1", "127.0.0.1:2049"},
		{"tcp6", "::1.3.255", "[::1]:1023"},
		{"tcp", "10.0.0.2.0.80", "10.0.0.2:80"},
	} {
		if got, err := parseUaddr4(tc.netid, tc.uaddr); err != nil || got != tc.want {
			t.Errorf("%s %q = %q, %v; want %q", tc.netid, tc.uaddr, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ netid, uaddr string }{
		{"udp", "127.0.0.1.8.1"},
		{"tcp", ""},
		{"tcp", "127.0.0.1"},
		{"tcp", "127.0.0.1.256.1"},
		{"tcp", "127.0.0.1.0.0"},
		{"tcp", "host.8.1"},
	} {
		if got, err := parseUaddr4(tc.netid, tc.uaddr); err == nil {
			t.Errorf("%s %q = %q, want an error", tc.netid, tc.uaddr, got)
		}
	}
}

// TestV4CallbackRedialsIdleConnection: clients close idle callback
// connections (macOS after 300 s), which the server learns only when it
// next calls. A recall over the closed connection is sent again on a new
// one rather than revoking the delegation and marking the path down.
func TestV4CallbackRedialsIdleConnection(t *testing.T) {
	fs, srv, addr := v4delegServer(t, nil)
	holder, cb := v4holder(t, srv, addr, "holder")
	cb.idleClose.Store(true) // the probe's connection closes after it
	v4write(t, fs, "f", "data")
	file := fs.FileID("f")
	for range 2 {
		id := holder.mustDelegate("o", "f")
		srv.Recall(file)
		cb.expectRecall(t, id, file)
		if s, data := holder.readWith("f", id); s != stOK || data != "data" {
			t.Fatalf("READ with the recalled delegation: status %d; it was revoked", s)
		}
		if s := holder.run(v4Renew(holder.client)).status; s != stOK {
			t.Fatalf("RENEW: status %d, want OK", s)
		}
		if s := holder.delegReturn(id); s != stOK {
			t.Fatalf("DELEGRETURN: status %d", s)
		}
		// Let the second round's recall not trip the thrash rule.
		srv.v4.mu.Lock()
		clear(srv.v4.recalled)
		srv.v4.mu.Unlock()
	}
}

// TestV4FailedFirstProbeIsRetried: a client whose first CB_NULL probe
// failed is probed again from RENEW, quietly: RENEW answers OK, since
// NFS4ERR_CB_PATH_DOWN would send macOS into state recovery.
func TestV4FailedFirstProbeIsRetried(t *testing.T) {
	var clock v4clock
	fs, srv, addr := v4delegServer(t, func(s *Server) { s.v4.now = clock.now })
	cb := newV4FakeCB(t)
	cb.failNull.Store(true)
	c := v4dial(t, addr)
	id := c.register("c", cb)
	v4wait(t, "the first probe to fail", func() bool { return !v4cbState(srv, id).cbProbing })
	v4write(t, fs, "f", "data")
	if o := c.mustOpen("o", "f", share4Read, false); o.deleg != nil {
		t.Fatal("delegation granted before a probe got through")
	}
	if s := c.run(v4Renew(id)).status; s != stOK {
		t.Fatalf("RENEW after a failed first probe: status %d, want OK", s)
	}
	cb.failNull.Store(false)
	clock.advance(v4ProbeEvery)
	if s := c.run(v4Renew(id)).status; s != stOK {
		t.Fatalf("RENEW: status %d, want OK", s)
	}
	v4wait(t, "the probe from RENEW", func() bool { return v4cbState(srv, id).cbUp })
	c.mustDelegate("o2", "f")
}

// TestV4QueuedRecallsSkipDownPath: recalls queued behind one whose callback
// failed revoke their delegations without calling over the path known to
// be down.
func TestV4QueuedRecallsSkipDownPath(t *testing.T) {
	fs, srv, addr := v4delegServer(t, nil)
	holder, cb := v4holder(t, srv, addr, "holder")
	v4write(t, fs, "f", "data")
	v4write(t, fs, "g", "data")
	idF := holder.mustDelegate("o", "f")
	idG := holder.mustDelegate("o", "g")
	gate := make(chan struct{})
	cb.gate.Store(&gate)
	cb.fail.Store(true)
	srv.Recall(fs.FileID("f"))
	v4wait(t, "the first CB_RECALL", func() bool { return cb.compounds.Load() == 1 })
	srv.Recall(fs.FileID("g"))
	close(gate)
	v4wait(t, "both delegations revoked", func() bool { return srv.v4.delegs.Load() == 0 })
	// The failed recall was sent twice: on the kept connection, then on a
	// new one. The queued one was not sent at all.
	if n := cb.compounds.Load(); n != 2 {
		t.Fatalf("%d CB_COMPOUNDs, want 2", n)
	}
	for _, tc := range []struct {
		name string
		id   v4StateID
	}{{"f", idF}, {"g", idG}} {
		if s, _ := holder.readWith(tc.name, tc.id); s != stBadStateID {
			t.Fatalf("READ of %s with the revoked delegation: status %d", tc.name, s)
		}
	}
}

// TestV4NamespaceChangeDuringGrant: a REMOVE or RENAME of the file while
// its delegation is being granted refuses the grant, as the completed
// change recalls delegations granted meanwhile.
func TestV4NamespaceChangeDuringGrant(t *testing.T) {
	var mu sync.Mutex
	var other *v4conn
	fs, srv, addr := v4delegServer(t, func(s *Server) {
		v4policy(s, func(_ *testFS, p string) bool {
			mu.Lock()
			defer mu.Unlock()
			var ops []v4op
			switch p {
			case "removed":
				ops = []v4op{v4Root(), v4Remove(p)}
			case "renamed":
				ops = []v4op{v4Root(), v4Save(), v4Rename(p, p+".new")}
			case "replaced":
				ops = []v4op{v4Root(), v4Save(), v4Rename("source", p)}
			default:
				return true
			}
			if s := other.run(ops...).status; s != stOK {
				t.Errorf("change of %s during the grant: status %d", p, s)
			}
			return true
		})
	})
	c, _ := v4holder(t, srv, addr, "c")
	o := v4dial(t, addr)
	o.setup(srv, "other", nil)
	mu.Lock()
	other = o
	mu.Unlock()
	for _, name := range []string{"removed", "renamed", "replaced", "source"} {
		v4write(t, fs, name, "data")
	}
	for _, name := range []string{"removed", "renamed", "replaced"} {
		if o := c.mustOpen("o", name, share4Read, false); o.deleg != nil {
			t.Errorf("delegation of %s granted though it changed while granted", name)
		}
	}
}

// TestV4OneChangeIsOneRecall: one change of a file two clients hold
// delegations of, and a recall by the delegation cap, count as one recall
// each for the thrash rule, or not at all: the file is delegated again.
func TestV4OneChangeIsOneRecall(t *testing.T) {
	fs, srv, addr := v4delegServer(t, nil)
	a, cbA := v4holder(t, srv, addr, "a")
	b, cbB := v4holder(t, srv, addr, "b")
	v4write(t, fs, "f", "data")
	file := fs.FileID("f")
	idA, idB := a.mustDelegate("o", "f"), b.mustDelegate("o", "f")
	srv.Recall(file)
	cbA.expectRecall(t, idA, file)
	cbB.expectRecall(t, idB, file)
	a.delegReturn(idA)
	b.delegReturn(idB)
	a.mustDelegate("o2", "f")
}

// TestV4CapRecallIsNoThrash: a recall the delegation cap makes is not a
// change of the file.
func TestV4CapRecallIsNoThrash(t *testing.T) {
	fs, srv, addr := v4delegServer(t, func(s *Server) { s.v4.maxDelegs = 1 })
	c, cb := v4holder(t, srv, addr, "c")
	v4write(t, fs, "a", "data")
	v4write(t, fs, "b", "data")
	a, b := fs.FileID("a"), fs.FileID("b")
	idA := c.mustDelegate("o", "a")
	idB := c.mustDelegate("o", "b")
	cb.expectRecall(t, idA, a) // by the cap
	c.delegReturn(idA)
	idA = c.mustDelegate("o2", "a")
	cb.expectRecall(t, idB, b) // by the cap
	c.delegReturn(idB)
	srv.Recall(a) // the one change of a
	cb.expectRecall(t, idA, a)
	c.delegReturn(idA)
	c.mustDelegate("o3", "a")
}
