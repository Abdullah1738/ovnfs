package nfs

import (
	"os"
	"testing"
)

// TestV4OpenStateIDGenerations checks stateid generations through OPEN and
// CLOSE: one state per owner and file, its seqid moving on with each change.
func TestV4OpenStateIDGenerations(t *testing.T) {
	srv, addr := v4serve(t, newTestFS(t), nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)

	first := c.mustOpen("alice", "f", share4AccessAll, true)
	if first.id.seqid != 1 {
		t.Fatalf("first open seqid %d, want 1", first.id.seqid)
	}
	if first.rflags != open4LocktypePosix {
		t.Fatalf("rflags %#x, want POSIX lock type only", first.rflags)
	}
	again := c.mustOpen("alice", "f", share4AccessAll, true)
	if again.id.other != first.id.other || again.id.seqid != 2 {
		t.Fatalf("reopen stateid %+v, want the same state at seqid 2", again.id)
	}
	if other := c.mustOpen("bob", "f", share4AccessAll, true); other.id.other == first.id.other || other.id.seqid != 1 {
		t.Fatalf("another owner's stateid %+v, want a new state at seqid 1", other.id)
	}
	if other := c.mustOpen("alice", "g", share4AccessAll, true); other.id.other == first.id.other {
		t.Fatal("another file must get its own stateid")
	}

	r := c.run(v4Root(), v4Close(again.id))
	r.skip(op4PutRootFH)
	if closed := getStateID4(r.next(op4Close, stOK)); closed.other != first.id.other || closed.seqid != 3 {
		t.Fatalf("CLOSE stateid %+v, want seqid 3", closed)
	}
	r.end()
	if s := c.run(v4Root(), v4Close(again.id)).status; s != stBadStateID {
		t.Fatalf("second CLOSE: status %d, want BAD_STATEID", s)
	}
}

// TestV4OpenConfirmAndDowngrade checks OPEN_CONFIRM and OPEN_DOWNGRADE
// results and argument checks.
func TestV4OpenConfirmAndDowngrade(t *testing.T) {
	srv, addr := v4serve(t, newTestFS(t), nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	o := c.mustOpen("o", "f", share4AccessAll, true)

	r := c.run(v4Root(), v4Look("f"), v4OpenConfirm(o.id), v4Downgrade(o.id, share4Read))
	r.skip(op4PutRootFH, op4Lookup)
	if id := getStateID4(r.next(op4OpenConfirm, stOK)); id.seqid != 2 || id.other != o.id.other {
		t.Fatalf("OPEN_CONFIRM stateid %+v", id)
	}
	if id := getStateID4(r.next(op4OpenDowngrade, stOK)); id.seqid != 3 || id.other != o.id.other {
		t.Fatalf("OPEN_DOWNGRADE stateid %+v", id)
	}
	r.end()
	for _, access := range []uint32{0, 4, share4Read | 8} {
		if s := c.run(v4Root(), v4Look("f"), v4Downgrade(o.id, access)).status; s != stInval {
			t.Errorf("downgrade to %#x: status %d, want INVAL", access, s)
		}
	}
	if s := c.run(v4Downgrade(o.id, share4Read)).status; s != stNoFileHandle {
		t.Errorf("downgrade without a filehandle: status %d", s)
	}
}

// TestV4OpenErrors checks the statuses OPEN refuses with.
func TestV4OpenErrors(t *testing.T) {
	fs := newTestFS(t)
	if err := os.Mkdir(fs.host("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", fs.host("l")); err != nil {
		t.Fatal(err)
	}
	v4write(t, fs, "f", "x")
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	open := func(a v4openArgs) status {
		if a.owner == (v4Owner{}) {
			a.owner = c.owner("o")
		}
		if a.access == 0 {
			a.access = share4Read
		}
		return c.run(v4Root(), v4Open(a)).status
	}
	for _, tc := range []struct {
		what string
		args v4openArgs
		want status
	}{
		{"directory", v4openArgs{name: "d"}, stIsDir},
		{"symlink", v4openArgs{name: "l"}, stSymlink},
		{"missing", v4openArgs{name: "missing"}, stNoEnt},
		{"deny", v4openArgs{name: "f", deny: 1}, stNotSupp},
		{"no access", v4openArgs{name: "f", access: 4}, stInval},
		{"guarded existing", v4openArgs{name: "f", create: true, how: create4Guarded}, stExist},
		{"bad create mode", v4openArgs{name: "f", create: true, how: 3}, stInval},
		{"unknown client", v4openArgs{name: "f", owner: v4Owner{client: 42, name: "o"}}, stStaleClientID},
		{"CLAIM_DELEGATE_PREV", v4openArgs{name: "f", claim: claim4DelegatePrev}, stNotSupp},
		{"CLAIM_DELEGATE_CUR of a missing file", v4openArgs{name: "missing", claim: claim4DelegateCur}, stNoEnt},
		{"CLAIM_DELEGATE_CUR creating", v4openArgs{name: "missing", create: true, claim: claim4DelegateCur}, stBadStateID},
		{"settable attribute", v4openArgs{name: "n", create: true, attrs: v4Attrs(bitmap4(attr4FileID), nil)}, stAttrNotSupp},
	} {
		if s := open(tc.args); s != tc.want {
			t.Errorf("%s: status %d, want %d", tc.what, s, tc.want)
		}
	}
	if _, err := os.Stat(fs.host("n")); err == nil {
		t.Error("OPEN with an unsettable attribute created the file")
	}
}

// TestV4ExclusiveCreateActsGuarded: with nowhere to keep EXCLUSIVE4's
// verifier, an existing file fails with NFS4ERR_EXIST.
func TestV4ExclusiveCreateActsGuarded(t *testing.T) {
	fs := newTestFS(t)
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	a := v4openArgs{owner: c.owner("o"), access: share4AccessAll, create: true, how: create4Exclusive, name: "new"}
	if s := c.run(v4Root(), v4Open(a)).status; s != stOK {
		t.Fatalf("exclusive create of a new file: status %d", s)
	}
	if _, err := os.Stat(fs.host("new")); err != nil {
		t.Fatalf("exclusive create made no file: %v", err)
	}
	if s := c.run(v4Root(), v4Open(a)).status; s != stExist {
		t.Fatalf("exclusive create of an existing file: status %d, want EXIST", s)
	}
}

// TestV4GuardedCreateAppliesAttributes checks that a create applies its
// attributes and reports them in attrset.
func TestV4GuardedCreateAppliesAttributes(t *testing.T) {
	fs := newTestFS(t)
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	a := v4openArgs{owner: c.owner("o"), access: share4AccessAll, create: true, how: create4Guarded, attrs: v4Mode(0o640), name: "f"}
	r := c.run(v4Root(), v4Open(a))
	r.skip(op4PutRootFH)
	if o := v4getOpened(t, r.next(op4Open, stOK)); o.attrset != bitmap4(attr4Mode) || !o.cinfo.atomic || o.cinfo.before == o.cinfo.after {
		t.Fatalf("attrset %v cinfo %+v", o.attrset, o.cinfo)
	}
	if info, err := os.Stat(fs.host("f")); err != nil || info.Mode().Perm() != hostPerm(0o640, false) {
		t.Fatalf("mode %v, %v; want 0640", info.Mode(), err)
	}
}

// TestV4UncheckedOpenLeavesExistingFile: UNCHECKED4 of an existing file
// applies no create attributes but a size of zero (RFC 7530 section
// 16.16.5), as open(O_CREAT) leaves an existing file's mode alone.
func TestV4UncheckedOpenLeavesExistingFile(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "f", "data")
	if err := os.Chmod(fs.host("f"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	open := func(attrs v4fattr) v4opened {
		a := v4openArgs{owner: c.owner("o"), access: share4AccessAll, create: true, attrs: attrs, name: "f"}
		r := c.run(v4Root(), v4Open(a))
		r.skip(op4PutRootFH)
		return v4getOpened(t, r.next(op4Open, stOK))
	}
	if o := open(v4Mode(0o644)); o.attrset != (v4bitmap{}) || o.cinfo.before != o.cinfo.after {
		t.Fatalf("attrset %v cinfo %+v", o.attrset, o.cinfo)
	}
	if info, _ := os.Stat(fs.host("f")); info.Mode().Perm() != hostPerm(0o755, false) || info.Size() != 4 {
		t.Fatalf("existing file changed: %v, %d bytes", info.Mode(), info.Size())
	}
	if o := open(v4Size(0)); o.attrset != bitmap4(attr4Size) {
		t.Fatalf("truncating open: attrset %v", o.attrset)
	}
	if info, _ := os.Stat(fs.host("f")); info.Size() != 0 {
		t.Fatalf("size %d after an UNCHECKED4 create with size 0", info.Size())
	}
}

// TestV4CreateAcceptsMacOSCreateAttrs: the macOS client masks the
// attributes it creates files with (mode, hidden, archive) by
// supported_attrs; whatever is left must be accepted, or every create
// fails.
func TestV4CreateAcceptsMacOSCreateAttrs(t *testing.T) {
	fs := newTestFS(t)
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	var mask v4bitmap
	var vals encoder
	for _, attr := range []uint32{14 /* archive */, 25 /* hidden */, attr4Mode} {
		if !attr4Supported.has(attr) {
			continue
		}
		mask[attr/32] |= 1 << (attr % 32)
		if attr == attr4Mode {
			vals.u32(0o644)
		} else {
			vals.bool(false)
		}
	}
	attrs := v4fattr{mask: mask, vals: vals.buf}
	a := v4openArgs{owner: c.owner("o"), access: share4AccessAll, create: true, how: create4Guarded, attrs: attrs, name: "f"}
	if s := c.run(v4Root(), v4Open(a)).status; s != stOK {
		t.Fatalf("OPEN create: status %d", s)
	}
	mkdir := v4Create(typeDir, "d", "", attrs)
	if s := c.run(v4Root(), mkdir).status; s != stOK {
		t.Fatalf("CREATE directory: status %d", s)
	}
	if info, err := os.Stat(fs.host("d")); err != nil || !info.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	if s := c.run(v4Root(), mkdir).status; s != stExist {
		t.Fatalf("CREATE of an existing directory: status %d, want EXIST", s)
	}
}

// TestV4WriteAfterChmodReadOnly: git creates an object, makes it read-only
// and then writes it through the descriptor it opened for writing.
func TestV4WriteAfterChmodReadOnly(t *testing.T) {
	fs := newTestFS(t)
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	a := v4openArgs{owner: c.owner("o"), access: share4AccessAll, create: true, how: create4Guarded, name: "obj"}
	r := c.run(v4Root(), v4Open(a), v4SetAttr(stateid4Anon, v4Mode(0o444)))
	r.skip(op4PutRootFH)
	o := v4getOpened(t, r.next(op4Open, stOK))
	if set, _ := getBitmap4(r.next(op4SetAttr, stOK)); set != bitmap4(attr4Mode) {
		t.Fatalf("attrsset %v", set)
	}
	r.end()

	if s := c.run(v4Root(), v4Look("obj"), v4Write(o.id, 2, "xy")).status; s != stOK {
		t.Fatalf("WRITE under the write open: status %d", s)
	}
	if data, _ := os.ReadFile(fs.host("obj")); string(data) != "\x00\x00xy" {
		t.Fatalf("obj = %q", data)
	}
	if os.Geteuid() != 0 {
		if s := c.run(v4Root(), v4Look("obj"), v4Write(stateid4Anon, 0, "z")).status; s != stAccess {
			t.Fatalf("WRITE without an open of the read-only file: status %d, want ACCESS", s)
		}
	}
	if s := c.run(v4Root(), v4Look("obj"), v4Close(o.id)).status; s != stOK {
		t.Fatalf("CLOSE: status %d", s)
	}
	srv.v4.mu.Lock()
	defer srv.v4.mu.Unlock()
	if len(srv.v4.files) != 0 {
		t.Fatalf("file state left after CLOSE: %v", srv.v4.files)
	}
}

// TestV4CreateReadOnlyKeepsWriting: a file created read-only by an open for
// writing stays writable through that open until it is downgraded.
func TestV4CreateReadOnlyKeepsWriting(t *testing.T) {
	fs := newTestFS(t)
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	a := v4openArgs{owner: c.owner("o"), access: share4AccessAll, create: true, how: create4Guarded, attrs: v4Mode(0o444), name: "ro"}
	r := c.run(v4Root(), v4Open(a))
	r.skip(op4PutRootFH)
	o := v4getOpened(t, r.next(op4Open, stOK))
	if info, _ := os.Stat(fs.host("ro")); info.Mode().Perm() != 0o444 {
		t.Fatalf("mode %v", info.Mode())
	}
	r = c.run(v4Root(), v4Look("ro"), v4Write(o.id, 0, "data"), v4SetAttr(o.id, v4Size(2)))
	if r.status != stOK {
		t.Fatalf("WRITE and truncate under the open: status %d", r.status)
	}
	if data, _ := os.ReadFile(fs.host("ro")); string(data) != "da" {
		t.Fatalf("ro = %q", data)
	}
	id := fs.FileID("ro")
	kept := func() bool {
		srv.v4.mu.Lock()
		defer srv.v4.mu.Unlock()
		return srv.v4.files[id] != nil && srv.v4.files[id].kept != nil
	}
	if !kept() {
		t.Fatal("no file kept for the write open")
	}
	if s := c.run(v4Root(), v4Look("ro"), v4Downgrade(o.id, share4Read)).status; s != stOK {
		t.Fatalf("OPEN_DOWNGRADE: status %d", s)
	}
	if kept() {
		t.Fatal("kept file outlived the last write open")
	}
	if os.Geteuid() != 0 {
		if s := c.run(v4Root(), v4Look("ro"), v4Write(o.id, 0, "x")).status; s != stAccess {
			t.Fatalf("WRITE after the downgrade: status %d, want ACCESS", s)
		}
	}
}

// TestV4DowngradeCannotWiden: OPEN_DOWNGRADE to access the open does not
// hold is NFS4ERR_INVAL (RFC 7530 section 16.19.4).
func TestV4DowngradeCannotWiden(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "f", "x")
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	o := c.mustOpen("o", "f", share4Read, false)
	for _, access := range []uint32{share4Write, share4AccessAll} {
		if s := c.run(v4Root(), v4Look("f"), v4Downgrade(o.id, access)).status; s != stInval {
			t.Errorf("downgrade of a read open to %#x: status %d, want INVAL", access, s)
		}
	}
	srv.v4.mu.Lock()
	defer srv.v4.mu.Unlock()
	if h := srv.v4.holds[o.id.other]; h.access != share4Read {
		t.Fatalf("open access %#x after refused downgrades", h.access)
	}
}
