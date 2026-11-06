package nfs

import (
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
)

// TestV4ReadWriteCommit checks the READ, WRITE and COMMIT result layouts:
// READ's eof, WRITE's count, FILE_SYNC and the instance's verifier.
func TestV4ReadWriteCommit(t *testing.T) {
	fs := newTestFS(t)
	if err := os.Mkdir(fs.host("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", fs.host("l")); err != nil {
		t.Fatal(err)
	}
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	o := c.mustOpen("o", "f", share4AccessAll, true)

	r := c.run(v4Root(), v4Look("f"), v4Write(o.id, 0, "hello"), v4Commit())
	r.skip(op4PutRootFH, op4Lookup)
	d := r.next(op4Write, stOK)
	if n, committed, verf := d.u32(), d.u32(), d.fixed(8); n != 5 || committed != stable4FileSync || string(verf) != string(srv.verf[:]) {
		t.Fatalf("WRITE count %d committed %d verf %x", n, committed, verf)
	}
	if verf := r.next(op4Commit, stOK).fixed(8); string(verf) != string(srv.verf[:]) {
		t.Fatalf("COMMIT verf %x", verf)
	}
	r.end()

	read := func(offset uint64, count uint32) (bool, string) {
		t.Helper()
		r := c.run(v4Root(), v4Look("f"), v4Read(o.id, offset, count))
		r.skip(op4PutRootFH, op4Lookup)
		d := r.next(op4Read, stOK)
		eof, data := d.bool(), d.opaque(100)
		r.end()
		return eof, string(data)
	}
	for _, tc := range []struct {
		offset uint64
		count  uint32
		eof    bool
		data   string
	}{
		{0, 100, true, "hello"},
		{0, 5, true, "hello"},
		{0, 3, false, "hel"},
		{3, 1, false, "l"},
		{0, 0, false, ""},
		{5, 10, true, ""},
		{1 << 62, 10, true, ""},
	} {
		if eof, data := read(tc.offset, tc.count); eof != tc.eof || data != tc.data {
			t.Errorf("READ %d+%d: eof %v data %q, want %v %q", tc.offset, tc.count, eof, data, tc.eof, tc.data)
		}
	}

	for _, tc := range []struct {
		what string
		ops  []v4op
		want status
	}{
		{"READ of a directory", []v4op{v4Root(), v4Look("d"), v4Read(stateid4Anon, 0, 1)}, stIsDir},
		{"READ of a symlink", []v4op{v4Root(), v4Look("l"), v4Read(stateid4Anon, 0, 1)}, stInval},
		{"WRITE of a directory", []v4op{v4Root(), v4Look("d"), v4Write(stateid4Bypass, 0, "x")}, stIsDir},
		{"WRITE of a symlink", []v4op{v4Root(), v4Look("l"), v4Write(stateid4Bypass, 0, "x")}, stInval},
		{"WRITE past the largest offset", []v4op{v4Root(), v4Look("f"), v4Write(o.id, 1<<63-1, "xy")}, stFBig},
		{"WRITE with a bad stable_how", []v4op{v4Root(), v4Look("f"), func(e *encoder) {
			e.u32(op4Write)
			putStateID4(e, o.id)
			e.u64(0)
			e.u32(3)
			e.opaque([]byte("x"))
		}}, stInval},
	} {
		if s := c.run(tc.ops...).status; s != tc.want {
			t.Errorf("%s: status %d, want %d", tc.what, s, tc.want)
		}
	}
	if data, _ := os.ReadFile(fs.host("f")); string(data) != "hello" {
		t.Fatalf("f = %q", data)
	}
}

// TestV4StateFromAnotherServerIsStale checks that client ids and stateids
// of another server instance are stale.
func TestV4StateFromAnotherServerIsStale(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "f", "")
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	if s := c.run(v4Renew(0x1234)).status; s != stStaleClientID {
		t.Fatalf("RENEW of an unknown client: status %d, want STALE_CLIENTID", s)
	}
	stale := v4StateID{seqid: 1, other: v4Other{1, 2, 3, 4, 0, 0, 0, 0, 0, 0, 0, 1}}
	srv.v4.mu.Lock()
	if stale.other[0] == byte(srv.v4.boot>>24) {
		stale.other[0]++
	}
	srv.v4.mu.Unlock()
	if s := c.run(v4Root(), v4Look("f"), v4Write(stale, 0, "x")).status; s != stStaleStateID {
		t.Fatalf("WRITE with another server's stateid: status %d, want STALE_STATEID", s)
	}
	if info, _ := os.Stat(fs.host("f")); info.Size() != 0 {
		t.Fatal("WRITE with a stale stateid changed the file")
	}
}

// TestV4SetAttr sets each settable attribute and checks the refusals.
func TestV4SetAttr(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "f", "hello")
	if err := os.Symlink("missing", fs.host("l")); err != nil {
		t.Fatal(err)
	}
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	mtime := time.Unix(1_600_000_000, 4_200) // NTFS keeps 100ns units
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	if runtime.GOOS == "windows" {
		uid, gid = 0, 0 // os.Getuid is -1, and a Windows host file has no owner
	}
	all := v4Attrs(bitmap4(attr4Size, attr4Mode, attr4Owner, attr4OwnerGroup, attr4TimeModifySet), func(e *encoder) {
		e.u64(2)
		e.u32(0o4600)
		e.str(strconv.FormatUint(uint64(uid), 10))
		e.str(strconv.FormatUint(uint64(gid), 10))
		e.u32(time4Client)
		putTime4(e, mtime)
	})
	r := c.run(v4Root(), v4Look("f"), v4SetAttr(stateid4Anon, all))
	r.skip(op4PutRootFH, op4Lookup)
	if set, _ := getBitmap4(r.next(op4SetAttr, stOK)); set != all.mask {
		t.Fatalf("attrsset %v", set)
	}
	r.end()
	info, err := os.Stat(fs.host("f"))
	// Windows keeps no setuid bit.
	if err != nil || info.Size() != 2 || info.Mode()&os.ModePerm != hostPerm(0o600, false) ||
		info.Mode()&os.ModeSetuid == 0 && runtime.GOOS != "windows" || !info.ModTime().Equal(mtime) {
		t.Fatalf("after SETATTR: %v, %v", info, err)
	}

	// The server's time is now; a symlink's mode is left alone.
	now := v4Attrs(bitmap4(attr4TimeModifySet), func(e *encoder) { e.u32(time4Server) })
	if s := c.run(v4Root(), v4Look("f"), v4SetAttr(stateid4Anon, now)).status; s != stOK {
		t.Fatalf("SETATTR to server time: status %d", s)
	}
	if info, _ := os.Stat(fs.host("f")); time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("mtime %v after SET_TO_SERVER_TIME4", info.ModTime())
	}
	if s := c.run(v4Root(), v4Look("l"), v4SetAttr(stateid4Anon, v4Mode(0o777))).status; s != stOK {
		t.Fatalf("SETATTR mode of a dangling symlink: status %d", s)
	}

	for _, tc := range []struct {
		what  string
		attrs v4fattr
		want  status
	}{
		{"unsettable attribute", v4Attrs(bitmap4(attr4FileID), func(e *encoder) { e.u64(1) }), stAttrNotSupp},
		{"unknown attribute", v4fattr{mask: v4bitmap{0, 1 << 30}}, stAttrNotSupp},
		{"named owner", v4Attrs(bitmap4(attr4Owner), func(e *encoder) { e.str("alice@example.com") }), stBadOwner},
		{"bad settime", v4Attrs(bitmap4(attr4TimeAccessSet), func(e *encoder) { e.u32(7) }), stInval},
		{"bad nseconds", v4Attrs(bitmap4(attr4TimeAccessSet), func(e *encoder) { e.u32(time4Client); e.u64(0); e.u32(1e9) }), stInval},
		{"short values", v4Attrs(bitmap4(attr4Size), func(e *encoder) { e.u32(1) }), stBadXDR},
		{"size of a directory", v4Size(0), stIsDir},
	} {
		target := "f"
		if tc.want == stIsDir {
			target = ""
		}
		ops := []v4op{v4Root()}
		if target != "" {
			ops = append(ops, v4Look(target))
		}
		r := c.run(append(ops, v4SetAttr(stateid4Anon, tc.attrs))...)
		if r.status != tc.want {
			t.Errorf("%s: status %d, want %d", tc.what, r.status, tc.want)
			continue
		}
		// SETATTR4res carries an empty attrsset on failure.
		r.skip(op4PutRootFH)
		if target != "" {
			r.skip(op4Lookup)
		}
		if set, _ := getBitmap4(r.next(op4SetAttr, tc.want)); set != (v4bitmap{}) {
			t.Errorf("%s: attrsset %v", tc.what, set)
		}
		r.end()
	}
}

// TestV4SetAttrFailureCarriesAttrsSet checks the failure layout: the status,
// then a bitmap4 of no words.
func TestV4SetAttrFailureCarriesAttrsSet(t *testing.T) {
	_, addr := v4serve(t, newTestFS(t), nil)
	c := v4dial(t, addr)
	r := c.run(v4Root(), v4SetAttr(stateid4Anon, v4Attrs(bitmap4(attr4FileID), nil)))
	if r.status != stAttrNotSupp {
		t.Fatalf("status %d, want ATTRNOTSUPP", r.status)
	}
	r.skip(op4PutRootFH)
	d := r.next(op4SetAttr, stAttrNotSupp)
	if n, a, b := d.u32(), d.u32(), d.u32(); n != 2 || a != 0 || b != 0 {
		t.Fatalf("attrsset %d words %#x %#x", n, a, b)
	}
	r.end()
}

// TestV4VerifyAndNVerify compares attributes encoded alike.
func TestV4VerifyAndNVerify(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "f", "data")
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	for _, tc := range []struct {
		op    uint32
		attrs v4fattr
		want  status
	}{
		{op4Verify, v4Size(4), stOK},
		{op4Verify, v4Size(5), stNotSame},
		{op4NVerify, v4Size(4), stSame},
		{op4NVerify, v4Size(5), stOK},
		{op4Verify, v4fattr{mask: bitmap4(attr4TimeModifySet)}, stInval},
		{op4Verify, v4fattr{mask: bitmap4(attr4RDAttrError)}, stInval},
		{op4Verify, v4fattr{mask: bitmap4(12 /* acl */)}, stAttrNotSupp},
	} {
		r := c.run(v4Root(), v4Look("f"), v4Verify(tc.op, tc.attrs), v4GetFH())
		if r.status != tc.want {
			t.Errorf("op %d mask %v: status %d, want %d", tc.op, tc.attrs.mask, r.status, tc.want)
			continue
		}
		r.skip(op4PutRootFH, op4Lookup)
		r.next(tc.op, tc.want)
		if tc.want == stOK {
			r.next(op4GetFH, stOK).fh(8)
		}
		r.end()
	}
}

// TestV4SymlinkAndHardLink makes a symlink with CREATE and a hard link with
// LINK.
func TestV4SymlinkAndHardLink(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "target", "data")
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)

	r := c.run(v4Root(), v4Create(typeLnk, "sl", "target", v4Mode(0o777)), v4ReadLink(), v4Root(),
		v4Create(typeLnk, "dangling", "missing", v4Mode(0o777)))
	r.skip(op4PutRootFH)
	d := r.next(op4Create, stOK)
	if ci := v4getCinfo(d); !ci.atomic {
		t.Fatalf("cinfo %+v", ci)
	}
	if set, _ := getBitmap4(d); set != bitmap4(attr4Mode) {
		t.Fatalf("attrset %v", set)
	}
	if target := r.next(op4ReadLink, stOK).str(100); target != "target" {
		t.Fatalf("READLINK %q", target)
	}
	r.skip(op4PutRootFH)
	d = r.next(op4Create, stOK)
	v4getCinfo(d)
	getBitmap4(d)
	r.end()
	if target, err := os.Readlink(fs.host("dangling")); err != nil || target != "missing" {
		t.Fatalf("dangling symlink %q, %v", target, err)
	}
	if s := c.run(v4Root(), v4Look("target"), v4ReadLink()).status; s != stInval {
		t.Fatalf("READLINK of a file: status %d, want INVAL", s)
	}

	link := []v4op{v4Root(), v4Look("target"), v4Save(), v4Root(), v4Link("hl")}
	r = c.run(link...)
	r.skip(op4PutRootFH, op4Lookup, op4SaveFH, op4PutRootFH)
	if ci := v4getCinfo(r.next(op4Link, stOK)); !ci.atomic || ci.before == ci.after {
		t.Fatalf("LINK cinfo %+v", ci)
	}
	r.end()
	a, _ := os.Stat(fs.host("target"))
	b, err := os.Stat(fs.host("hl"))
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("hl is not a hard link of target: %v", err)
	}
	if s := c.run(link...).status; s != stExist {
		t.Fatalf("LINK over an existing name: status %d, want EXIST", s)
	}
	if s := c.run(v4Root(), v4Link("x")).status; s != stNoFileHandle {
		t.Fatalf("LINK without a saved filehandle: status %d", s)
	}
	if s := c.run(v4Root(), v4Save(), v4Link("x")).status; s != stIsDir {
		t.Fatalf("LINK of a directory: status %d", s)
	}
}

// TestV4OpenAttrAndDelegPurgeAreNotSupported checks they are NOTSUPP, not
// OP_ILLEGAL.
func TestV4OpenAttrAndDelegPurgeAreNotSupported(t *testing.T) {
	_, addr := v4serve(t, newTestFS(t), nil)
	c := v4dial(t, addr)
	for _, op := range []v4op{
		func(e *encoder) { e.u32(op4OpenAttr); e.bool(false) },
		func(e *encoder) { e.u32(op4DelegPurge); e.u64(1) },
	} {
		r := c.run(v4Root(), op)
		if r.status != stNotSupp {
			t.Fatalf("status %d, want NOTSUPP", r.status)
		}
		r.skip(op4PutRootFH)
		var probe encoder
		op(&probe)
		r.next(v4firstOp(probe.buf), stNotSupp)
		r.end()
	}
}

// v4firstOp is the operation number an encoded operation starts with.
func v4firstOp(b []byte) uint32 {
	d := decoder{buf: b}
	return d.u32()
}

// TestV4ChangeMovesOnModeAndLinks: the change attribute moves with every
// change clients cache attributes of, not only size and modification time.
func TestV4ChangeMovesOnModeAndLinks(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "f", "data")
	srv := &Server{FS: fs}
	mtime := time.Unix(1_700_000_000, 0)
	pin := func() {
		if err := os.Chtimes(fs.host("f"), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	pin()
	before := srv.changeAt4("f")
	// Read-only, as a Windows host keeps no other mode.
	if err := os.Chmod(fs.host("f"), 0o444); err != nil {
		t.Fatal(err)
	}
	pin()
	afterChmod := srv.changeAt4("f")
	if afterChmod == before {
		t.Fatal("change did not move on chmod")
	}
	if err := os.Link(fs.host("f"), fs.host("g")); err != nil {
		t.Fatal(err)
	}
	pin()
	// Go reports no link count of a Windows host file.
	if srv.changeAt4("f") == afterChmod && runtime.GOOS != "windows" {
		t.Fatal("change did not move on a new hard link")
	}
	if srv.changeAt4("missing") != 0 {
		t.Fatal("change of a missing file")
	}
}

// TestV4StateIDOfAnotherFile: READ, WRITE and SETATTR with a stateid of
// another file are NFS4ERR_BAD_STATEID and change neither file.
func TestV4StateIDOfAnotherFile(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "a", "AAAA")
	v4write(t, fs, "b", "BBBB")
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	// a is read-only and open for writing, so its open keeps a file.
	o := c.mustOpen("o", "a", share4AccessAll, false)
	c.mustOpen("o", "b", share4AccessAll, false)
	if s := c.run(v4Root(), v4Look("a"), v4SetAttr(o.id, v4Mode(0o444))).status; s != stOK {
		t.Fatalf("SETATTR: status %d", s)
	}
	for what, op := range map[string]v4op{
		"READ":    v4Read(o.id, 0, 4),
		"WRITE":   v4Write(o.id, 0, "xx"),
		"SETATTR": v4SetAttr(o.id, v4Size(1)),
	} {
		if s := c.run(v4Root(), v4Look("b"), op).status; s != stBadStateID {
			t.Errorf("%s of b with a's stateid: status %d, want BAD_STATEID", what, s)
		}
	}
	for name, want := range map[string]string{"a": "AAAA", "b": "BBBB"} {
		if data, _ := os.ReadFile(fs.host(name)); string(data) != want {
			t.Errorf("%s = %q", name, data)
		}
	}
}

// TestV4WriteUnderLockStateIDAfterChmod: clients holding byte-range locks
// write with their lock stateid, which serves the kept file of their write
// open as the open stateid does.
func TestV4WriteUnderLockStateIDAfterChmod(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes read-only files")
	}
	fs := newTestFS(t)
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	o := c.mustOpen("o", "f", share4AccessAll, true)
	r := c.run(v4Root(), v4Look("f"),
		v4Lock(v4lockArgs{typ: lock4Write, length: length4EOF, fresh: true, open: o.id, owner: c.owner("l")}),
		v4SetAttr(stateid4Anon, v4Mode(0o444)))
	r.skip(op4PutRootFH, op4Lookup)
	lock := getStateID4(r.next(op4Lock, stOK))
	r.next(op4SetAttr, stOK)
	if s := c.run(v4Root(), v4Look("f"), v4Write(lock, 0, "data")).status; s != stOK {
		t.Fatalf("WRITE under the lock stateid: status %d", s)
	}
	if data, _ := os.ReadFile(fs.host("f")); string(data) != "data" {
		t.Fatalf("f = %q", data)
	}
}

// v4racyFS runs onOpen before opening a file for writing only, as WRITE
// does.
type v4racyFS struct {
	*testFS
	onOpen func(p string)
}

// OpenFile runs the hook, then opens.
func (fs *v4racyFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR) == os.O_WRONLY && fs.onOpen != nil {
		fs.onOpen(p)
	}
	return fs.testFS.OpenFile(p, flag, perm)
}

// TestV4WriteRacingChmod: a SETATTR taking the owner's write permission
// between a WRITE's stateid check and its open leaves a kept file, which
// the WRITE then uses.
func TestV4WriteRacingChmod(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes read-only files")
	}
	fs := &v4racyFS{testFS: newTestFS(t)}
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	other := v4dial(t, addr)
	o := c.mustOpen("o", "f", share4AccessAll, true)
	var once sync.Once
	fs.onOpen = func(string) {
		once.Do(func() {
			if s := other.run(v4Root(), v4Look("f"), v4SetAttr(stateid4Anon, v4Mode(0o444))).status; s != stOK {
				t.Errorf("SETATTR: status %d", s)
			}
		})
	}
	if s := c.run(v4Root(), v4Look("f"), v4Write(o.id, 0, "data")).status; s != stOK {
		t.Fatalf("WRITE racing the chmod: status %d", s)
	}
	if data, _ := os.ReadFile(fs.host("f")); string(data) != "data" {
		t.Fatalf("f = %q", data)
	}
}
