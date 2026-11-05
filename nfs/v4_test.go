package nfs

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// A hand-rolled NFSv4.0 COMPOUND client for the tests: v4op encodes one
// operation, v4conn.run sends them on one connection and v4res walks the
// results.

// v4op encodes one operation of a COMPOUND.
type v4op func(e *encoder)

// v4conn is one NFSv4 client connection.
type v4conn struct {
	t      testing.TB
	rc     *rpcClient
	client uint64 // the client id, once set up
}

// v4dial opens a connection to the server at addr.
func v4dial(t testing.TB, addr string) *v4conn {
	return &v4conn{t: t, rc: dial(t, addr)}
}

// v4res is a COMPOUND reply being read.
type v4res struct {
	t      testing.TB
	d      *decoder
	status status
	tag    string
	n      uint32
}

// run sends a COMPOUND with tag "t".
func (c *v4conn) run(ops ...v4op) *v4res {
	c.t.Helper()
	return c.runTagged("t", 0, ops...)
}

// runTagged sends a COMPOUND with the given tag and minor version.
func (c *v4conn) runTagged(tag string, minor uint32, ops ...v4op) *v4res {
	c.t.Helper()
	stat, d := c.rc.call(progNFS, 4, proc4Compound, func(e *encoder) {
		e.str(tag)
		e.u32(minor)
		e.u32(uint32(len(ops)))
		for _, op := range ops {
			op(e)
		}
	})
	if stat != rpcSuccess {
		c.t.Fatalf("COMPOUND: accept_stat %d", stat)
	}
	r := &v4res{t: c.t, d: d, status: status(d.u32())}
	r.tag = d.str(v4OpaqueLimit)
	r.n = d.u32()
	if d.err != nil {
		c.t.Fatal("truncated COMPOUND reply")
	}
	return r
}

// next checks that the next result is op's with status want, and returns
// the decoder at its body.
func (r *v4res) next(op uint32, want status) *decoder {
	r.t.Helper()
	gotOp, got := r.d.u32(), status(r.d.u32())
	if r.d.err != nil {
		r.t.Fatalf("no result for op %d", op)
	}
	if gotOp != op || got != want {
		r.t.Fatalf("result op %d status %d, want op %d status %d", gotOp, got, op, want)
	}
	return r.d
}

// skip checks that the next results are those ops' with NFS4_OK and no
// body.
func (r *v4res) skip(ops ...uint32) {
	r.t.Helper()
	for _, op := range ops {
		r.next(op, stOK)
	}
}

// end checks that the reply holds nothing more.
func (r *v4res) end() {
	r.t.Helper()
	if r.d.err != nil || len(r.d.buf) != 0 {
		r.t.Fatalf("reply ends badly: %v, %d bytes left", r.d.err, len(r.d.buf))
	}
}

// Operation builders.

func v4Root() v4op           { return func(e *encoder) { e.u32(op4PutRootFH) } }
func v4GetFH() v4op          { return func(e *encoder) { e.u32(op4GetFH) } }
func v4Save() v4op           { return func(e *encoder) { e.u32(op4SaveFH) } }
func v4Restore() v4op        { return func(e *encoder) { e.u32(op4RestoreFH) } }
func v4LookP() v4op          { return func(e *encoder) { e.u32(op4LookupP) } }
func v4ReadLink() v4op       { return func(e *encoder) { e.u32(op4ReadLink) } }
func v4PutFH(id uint64) v4op { return func(e *encoder) { e.u32(op4PutFH); e.fh(id) } }

func v4Look(name string) v4op   { return v4NameOp(op4Lookup, name) }
func v4Remove(name string) v4op { return v4NameOp(op4Remove, name) }
func v4Link(name string) v4op   { return v4NameOp(op4Link, name) }

// v4NameOp is an operation whose argument is one name.
func v4NameOp(op uint32, name string) v4op {
	return func(e *encoder) { e.u32(op); e.str(name) }
}

func v4Rename(from, to string) v4op {
	return func(e *encoder) { e.u32(op4Rename); e.str(from); e.str(to) }
}

func v4GetAttr(attrs ...uint32) v4op {
	return func(e *encoder) { e.u32(op4GetAttr); bitmap4(attrs...).put(e) }
}

func v4Access(bits uint32) v4op {
	return func(e *encoder) { e.u32(op4Access); e.u32(bits) }
}

// v4fattr is a fattr4 a test sends.
type v4fattr struct {
	mask v4bitmap
	vals []byte
}

// put appends the fattr4.
func (f v4fattr) put(e *encoder) {
	f.mask.put(e)
	e.opaque(f.vals)
}

// v4Attrs builds a fattr4 whose values vals encodes.
func v4Attrs(mask v4bitmap, vals func(*encoder)) v4fattr {
	var e encoder
	if vals != nil {
		vals(&e)
	}
	return v4fattr{mask: mask, vals: e.buf}
}

// v4Mode is a fattr4 setting the mode.
func v4Mode(mode uint32) v4fattr {
	return v4Attrs(bitmap4(attr4Mode), func(e *encoder) { e.u32(mode) })
}

// v4Size is a fattr4 setting or holding the size.
func v4Size(size uint64) v4fattr {
	return v4Attrs(bitmap4(attr4Size), func(e *encoder) { e.u64(size) })
}

func v4SetAttr(id v4StateID, f v4fattr) v4op {
	return func(e *encoder) { e.u32(op4SetAttr); putStateID4(e, id); f.put(e) }
}

func v4Verify(op uint32, f v4fattr) v4op {
	return func(e *encoder) { e.u32(op); f.put(e) }
}

func v4Create(typ fileType, name, target string, f v4fattr) v4op {
	return func(e *encoder) {
		e.u32(op4Create)
		e.u32(uint32(typ))
		switch typ {
		case typeLnk:
			e.str(target)
		case typeBlk, typeChr:
			e.u32(1)
			e.u32(2)
		}
		e.str(name)
		f.put(e)
	}
}

func v4Read(id v4StateID, offset uint64, count uint32) v4op {
	return func(e *encoder) { e.u32(op4Read); putStateID4(e, id); e.u64(offset); e.u32(count) }
}

func v4Write(id v4StateID, offset uint64, data string) v4op {
	return func(e *encoder) {
		e.u32(op4Write)
		putStateID4(e, id)
		e.u64(offset)
		e.u32(stable4FileSync)
		e.opaque([]byte(data))
	}
}

func v4Commit() v4op { return func(e *encoder) { e.u32(op4Commit); e.u64(0); e.u32(0) } }

func v4Close(id v4StateID) v4op {
	return func(e *encoder) { e.u32(op4Close); e.u32(1); putStateID4(e, id) }
}

func v4OpenConfirm(id v4StateID) v4op {
	return func(e *encoder) { e.u32(op4OpenConfirm); putStateID4(e, id); e.u32(1) }
}

func v4Downgrade(id v4StateID, access uint32) v4op {
	return func(e *encoder) { e.u32(op4OpenDowngrade); putStateID4(e, id); e.u32(1); e.u32(access); e.u32(0) }
}

func v4DelegReturn(id v4StateID) v4op {
	return func(e *encoder) { e.u32(op4DelegReturn); putStateID4(e, id) }
}

func v4Renew(client uint64) v4op {
	return func(e *encoder) { e.u32(op4Renew); e.u64(client) }
}

func v4ReadDir(cookie, verf uint64, maxcount uint32, attrs ...uint32) v4op {
	return func(e *encoder) {
		e.u32(op4ReadDir)
		e.u64(cookie)
		e.u64(verf)
		e.u32(maxcount)
		e.u32(maxcount)
		bitmap4(attrs...).put(e)
	}
}

// v4lockArgs are a LOCK's arguments.
type v4lockArgs struct {
	typ            uint32
	reclaim        bool
	offset, length uint64
	// open and owner introduce a new lock owner; otherwise lockID is the
	// owner's lock stateid.
	fresh  bool
	open   v4StateID
	owner  v4Owner
	lockID v4StateID
}

func v4Lock(a v4lockArgs) v4op {
	return func(e *encoder) {
		e.u32(op4Lock)
		e.u32(a.typ)
		e.bool(a.reclaim)
		e.u64(a.offset)
		e.u64(a.length)
		e.bool(a.fresh)
		if a.fresh {
			e.u32(1)
			putStateID4(e, a.open)
			e.u32(1)
			e.u64(a.owner.client)
			e.str(a.owner.name)
		} else {
			putStateID4(e, a.lockID)
			e.u32(2)
		}
	}
}

func v4LockT(typ uint32, offset, length uint64, owner v4Owner) v4op {
	return func(e *encoder) {
		e.u32(op4LockT)
		e.u32(typ)
		e.u64(offset)
		e.u64(length)
		e.u64(owner.client)
		e.str(owner.name)
	}
}

func v4LockU(typ uint32, id v4StateID, offset, length uint64) v4op {
	return func(e *encoder) {
		e.u32(op4LockU)
		e.u32(typ)
		e.u32(1)
		putStateID4(e, id)
		e.u64(offset)
		e.u64(length)
	}
}

// v4openArgs are an OPEN's arguments; the zero value opens name with
// CLAIM_NULL without creating.
type v4openArgs struct {
	owner     v4Owner
	access    uint32
	deny      uint32
	create    bool
	how       uint32
	attrs     v4fattr
	claim     uint32
	name      string
	delegType uint32
	delegID   v4StateID
}

func v4Open(a v4openArgs) v4op {
	return func(e *encoder) {
		e.u32(op4Open)
		e.u32(1) // seqid
		e.u32(a.access)
		e.u32(a.deny)
		e.u64(a.owner.client)
		e.str(a.owner.name)
		if a.create {
			e.u32(open4Create)
			e.u32(a.how)
			if a.how == create4Exclusive {
				e.fixed([]byte("verifier"))
			} else {
				a.attrs.put(e)
			}
		} else {
			e.u32(open4NoCreate)
		}
		e.u32(a.claim)
		switch a.claim {
		case claim4Null, claim4DelegatePrev:
			e.str(a.name)
		case claim4Previous:
			e.u32(a.delegType)
		case claim4DelegateCur:
			putStateID4(e, a.delegID)
			e.str(a.name)
		}
	}
}

// v4opened is an OPEN result.
type v4opened struct {
	id      v4StateID
	cinfo   v4cinfo
	rflags  uint32
	attrset v4bitmap
	deleg   *v4StateID
}

// v4getOpened decodes an OPEN4resok, checking a read delegation's layout.
func v4getOpened(t testing.TB, d *decoder) v4opened {
	t.Helper()
	o := v4opened{id: getStateID4(d), cinfo: v4getCinfo(d), rflags: d.u32()}
	o.attrset, _ = getBitmap4(d)
	switch typ := d.u32(); typ {
	case deleg4None:
	case deleg4Read:
		id := getStateID4(d)
		recall := d.bool()
		aceType, aceFlag, aceMask, who := d.u32(), d.u32(), d.u32(), d.str(v4OpaqueLimit)
		if recall || aceType != ace4AccessAllow || aceFlag != 0 || aceMask != 0 || who != "" {
			t.Fatalf("read delegation recall %v ace %d/%d/%d %q", recall, aceType, aceFlag, aceMask, who)
		}
		o.deleg = &id
	default:
		t.Fatalf("delegation type %d", typ)
	}
	if d.err != nil {
		t.Fatal("truncated OPEN result")
	}
	return o
}

// v4getCinfo decodes a change_info4.
func v4getCinfo(d *decoder) v4cinfo {
	return v4cinfo{atomic: d.bool(), before: d.u64(), after: d.u64()}
}

// v4ownerOf is the open owner name of conn's client.
func (c *v4conn) owner(name string) v4Owner { return v4Owner{client: c.client, name: name} }

// open opens name in the root directory with the given share access as
// owner, creating it UNCHECKED if create, and returns the status and, on
// success, the result.
func (c *v4conn) open(owner, name string, access uint32, create bool) (status, v4opened) {
	c.t.Helper()
	a := v4openArgs{owner: c.owner(owner), access: access, create: create, name: name}
	r := c.run(v4Root(), v4Open(a))
	if r.status != stOK {
		return r.status, v4opened{}
	}
	r.skip(op4PutRootFH)
	return stOK, v4getOpened(c.t, r.next(op4Open, stOK))
}

// mustOpen is open that must succeed.
func (c *v4conn) mustOpen(owner, name string, access uint32, create bool) v4opened {
	c.t.Helper()
	s, o := c.open(owner, name, access, create)
	if s != stOK {
		c.t.Fatalf("OPEN %s: status %d", name, s)
	}
	return o
}

// setup makes the connection's client id, with cb as its callback service
// (nil for none), and waits for the server's probe of it to get through.
func (c *v4conn) setup(srv *Server, name string, cb *v4FakeCB) uint64 {
	c.t.Helper()
	id := c.register(name, cb)
	if cb != nil {
		v4wait(c.t, "callback probe", func() bool { return v4cbState(srv, id).cbUp })
	}
	return id
}

// register makes and confirms the connection's client id, with cb as its
// callback service (nil for none), without waiting for the probe.
func (c *v4conn) register(name string, cb *v4FakeCB) uint64 {
	c.t.Helper()
	r := c.run(func(e *encoder) {
		e.u32(op4SetClientID)
		e.fixed([]byte("bootverf"))
		e.str(name)
		e.u32(v4CBProg)
		if cb != nil {
			e.str("tcp")
			e.str(cb.uaddr())
		} else {
			e.str("")
			e.str("")
		}
		e.u32(7) // callback_ident
	})
	d := r.next(op4SetClientID, stOK)
	id := d.u64()
	confirm := d.fixed(8)
	if s := c.confirm(id, confirm); s != stOK {
		c.t.Fatalf("SETCLIENTID_CONFIRM: status %d", s)
	}
	c.client = id
	return id
}

// v4cbState is a client's callback path state.
func v4cbState(srv *Server, id uint64) (s struct{ cbUp, cbDown, cbProbing bool }) {
	srv.v4.mu.Lock()
	defer srv.v4.mu.Unlock()
	c := srv.v4.clients[id]
	s.cbUp, s.cbDown, s.cbProbing = c.cbUp, c.cbDown, c.cbProbing
	return s
}

// confirm sends SETCLIENTID_CONFIRM.
func (c *v4conn) confirm(id uint64, confirm []byte) status {
	c.t.Helper()
	return c.run(func(e *encoder) { e.u32(op4SetClientIDConfirm); e.u64(id); e.fixed(confirm) }).status
}

// v4wait waits up to 5 s for cond.
func v4wait(t testing.TB, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// v4clock is a clock the test moves on.
type v4clock struct{ offset atomic.Int64 }

// now is the clock's time.
func (c *v4clock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

// advance moves the clock on.
func (c *v4clock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// v4serve starts a server over fs, with tune adjusting it before it
// starts, and returns it and its address.
func v4serve(t testing.TB, fs Backend, tune func(*Server)) (*Server, string) {
	srv := &Server{FS: fs, Concurrency: 8}
	if tune != nil {
		tune(srv)
	}
	return srv, startServer(t, srv)
}

// v4write writes a file of the test filesystem directly.
func v4write(t testing.TB, fs *testFS, name, data string) {
	t.Helper()
	if err := os.WriteFile(fs.host(name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// v4CBProg is the callback program number the tests give.
const v4CBProg = 0x40000000

// v4FakeCB is a client's NFSv4.0 callback service: it answers CB_NULL and
// reports the CB_RECALLs it gets; with fail set it drops the connection on
// CB_COMPOUND instead. While unknown is positive it answers that many
// CB_RECALLs NFS4ERR_BAD_STATEID, as a client that has not yet processed
// the OPEN reply granting the delegation does.
type v4FakeCB struct {
	l       net.Listener
	recalls chan v4FakeRecall
	fail    atomic.Bool
	unknown atomic.Int32
	// failNull drops the connection on CB_NULL, failing probes;
	// idleClose closes the connection after every reply, as a client
	// closing an idle callback connection does before the next call.
	failNull  atomic.Bool
	idleClose atomic.Bool
	// compounds counts the CB_COMPOUNDs received; a CB_COMPOUND waits for
	// gate, if set, to close before it is answered.
	compounds atomic.Int32
	gate      atomic.Pointer[chan struct{}]
}

// v4FakeRecall is a CB_RECALL received.
type v4FakeRecall struct {
	ident    uint32
	id       v4StateID
	truncate bool
	file     uint64
	fhOK     bool
}

// newV4FakeCB starts a callback service on loopback.
func newV4FakeCB(t testing.TB) *v4FakeCB {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &v4FakeCB{l: l, recalls: make(chan v4FakeRecall, 64)}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(nc)
		}
	}()
	return f
}

// serve answers one connection's calls.
func (f *v4FakeCB) serve(nc net.Conn) {
	defer nc.Close()
	r := bufio.NewReader(nc)
	for {
		rec, err := readRecord(r, nil)
		if err != nil {
			return
		}
		d := decoder{buf: rec}
		xid, mtype, vers, prog, pvers, proc := d.u32(), d.u32(), d.u32(), d.u32(), d.u32(), d.u32()
		flavor := d.u32()
		cred := decoder{buf: d.opaque(maxAuthBytes)}
		d.u32()
		d.opaque(maxAuthBytes)
		if d.err != nil || mtype != msgCall || vers != 2 || prog != v4CBProg || pvers != cb4Version {
			return
		}
		e := encoder{}
		e.u32(0)
		e.u32(xid)
		e.u32(msgReply)
		// Like Linux's nfs_callback_authenticate: AUTH_NULL only for
		// CB_NULL, and an AUTH_SYS credential that decodes.
		authOK := flavor == authNone && proc == cb4Null
		if flavor == authSys {
			cred.u32() // stamp
			machine := cred.str(255)
			cred.u32() // uid
			cred.u32() // gid
			cred.opaque(16 * 4)
			authOK = cred.err == nil && len(cred.buf) == 0 && machine != ""
		}
		if proc == cb4Null && f.failNull.Load() {
			return
		}
		if !authOK {
			e.u32(replyDenied)
			e.u32(rejectAuthError)
			e.u32(5) // AUTH_TOOWEAK
		} else {
			e.u32(replyAccepted)
			e.u32(authNone)
			e.u32(0)
			e.u32(uint32(rpcSuccess))
		}
		if authOK && proc == cb4Compound {
			f.compounds.Add(1)
			if g := f.gate.Load(); g != nil {
				<-*g
			}
			if f.fail.Load() {
				return
			}
			tag := d.str(v4OpaqueLimit)
			minor, ident, n, op := d.u32(), d.u32(), d.u32(), d.u32()
			rc := v4FakeRecall{ident: ident, id: getStateID4(&d), truncate: d.bool()}
			rc.file, rc.fhOK = d.fh(128)
			if d.err != nil || minor != 0 || n != 1 || op != cb4OpRecall || len(d.buf) != 0 {
				return
			}
			f.recalls <- rc
			s := stOK
			if f.unknown.Add(-1) >= 0 {
				s = stBadStateID
			}
			e.u32(uint32(s))
			e.str(tag)
			e.u32(1)
			e.u32(cb4OpRecall)
			e.u32(uint32(s))
		}
		e.put32(0, lastFragment|uint32(e.len()-4))
		if _, err := nc.Write(e.buf); err != nil {
			return
		}
		if f.idleClose.Load() {
			// As a client closing the idle connection after the call.
			return
		}
	}
}

// uaddr is the service's universal address, as SETCLIENTID carries it.
func (f *v4FakeCB) uaddr() string {
	a := f.l.Addr().(*net.TCPAddr)
	return fmt.Sprintf("%s.%d.%d", a.IP, a.Port>>8, a.Port&0xff)
}

// expectRecall waits for a CB_RECALL of delegation id of file.
func (f *v4FakeCB) expectRecall(t testing.TB, id v4StateID, file uint64) {
	t.Helper()
	select {
	case r := <-f.recalls:
		if r.ident != 7 || r.id != id || !r.fhOK || r.file != file || r.truncate {
			t.Fatalf("CB_RECALL %+v, want ident 7, stateid %+v, file %d", r, id, file)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no CB_RECALL of %+v", id)
	}
}

// expectNoRecall checks that no CB_RECALL comes for a while.
func (f *v4FakeCB) expectNoRecall(t testing.TB) {
	t.Helper()
	select {
	case r := <-f.recalls:
		t.Fatalf("unexpected CB_RECALL %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}
