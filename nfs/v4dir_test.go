package nfs

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// v4entry is a READDIR entry as decoded.
type v4entry struct {
	cookie uint64
	name   string
	fileid uint64
}

// v4readdir reads one READDIR page of the root with the fileid attribute.
func v4readdir(t *testing.T, c *v4conn, cookie, verf uint64, maxcount uint32) (status, uint64, []v4entry, bool) {
	t.Helper()
	r := c.run(v4Root(), v4ReadDir(cookie, verf, maxcount, attr4FileID))
	if r.status != stOK {
		return r.status, 0, nil, false
	}
	r.skip(op4PutRootFH)
	d := r.next(op4ReadDir, stOK)
	verf = d.u64()
	var entries []v4entry
	for d.bool() {
		e := v4entry{cookie: d.u64(), name: d.str(255)}
		if mask, _ := getBitmap4(d); mask != bitmap4(attr4FileID) || d.u32() != 8 {
			t.Fatalf("entry %q attributes", e.name)
		}
		e.fileid = d.u64()
		entries = append(entries, e)
	}
	eof := d.bool()
	r.end()
	return stOK, verf, entries, eof
}

// TestV4ReadDir pages through a directory: entries sorted by name with
// cookies from firstCookie, a continuation by cookie and verifier, and the
// statuses for a changed directory and a cookie never issued.
func TestV4ReadDir(t *testing.T) {
	fs := newTestFS(t)
	names := []string{"c", "a", "e", "b", "d"}
	for _, n := range names {
		v4write(t, fs, n, n)
	}
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)

	s, verf, all, eof := v4readdir(t, c, 0, 0, 4096)
	if s != stOK || !eof || len(all) != 5 || verf == 0 {
		t.Fatalf("status %d eof %v %d entries verf %x", s, eof, len(all), verf)
	}
	for i, e := range all {
		if want := string(rune('a' + i)); e.name != want || e.cookie != firstCookie+uint64(i) || e.fileid != fs.FileID(want) {
			t.Fatalf("entry %d = %+v", i, e)
		}
	}

	// Pages of two entries: the verifier (8 bytes), entries of 44 (follows
	// 4, cookie 8, name 8, fattr4 12+4+8), and 8 to end the list.
	var paged []v4entry
	cookie := uint64(0)
	pageVerf := uint64(0)
	for range 10 {
		s, v, entries, eof := v4readdir(t, c, cookie, pageVerf, 8+2*44+8)
		if s != stOK || len(entries) == 0 || len(entries) > 2 {
			t.Fatalf("page after %d: status %d, %d entries", cookie, s, len(entries))
		}
		paged = append(paged, entries...)
		cookie, pageVerf = entries[len(entries)-1].cookie, v
		if eof {
			break
		}
	}
	if fmt.Sprint(paged) != fmt.Sprint(all) {
		t.Fatalf("paged %v, want %v", paged, all)
	}

	// Names alone come with empty attributes.
	r := c.run(v4Root(), v4ReadDir(0, 0, 4096, attr4TimeModifySet))
	r.skip(op4PutRootFH)
	d := r.next(op4ReadDir, stOK)
	d.u64()
	for _, want := range all {
		if !d.bool() || d.u64() != want.cookie || d.str(10) != want.name {
			t.Fatalf("names-only entry, want %+v", want)
		}
		if mask, _ := getBitmap4(d); mask != (v4bitmap{}) || d.u32() != 0 {
			t.Fatal("names-only entry has attributes")
		}
	}
	if d.bool() || !d.bool() {
		t.Fatal("names-only list end")
	}
	r.end()

	if s, _, _, _ := v4readdir(t, c, 0, 0, 8+43+8); s != stTooSmall {
		t.Fatalf("maxcount below one entry: status %d, want TOOSMALL", s)
	}
	if s, _, _, _ := v4readdir(t, c, firstCookie+10, verf, 4096); s != stBadCookie {
		t.Fatalf("cookie never issued: status %d, want BAD_COOKIE", s)
	}
	// A continuation whose listing is gone and whose directory changed:
	// the verifier is no longer valid (RFC 7530 section 16.24.4).
	v4write(t, fs, "f", "f")
	if s, _, _, _ := v4readdir(t, c, firstCookie, verf^1, 4096); s != stNotSame {
		t.Fatalf("changed directory: status %d, want NOT_SAME", s)
	}
	if s := c.run(v4Root(), v4Look("a"), v4ReadDir(0, 0, 4096)).status; s != stNotDir {
		t.Fatalf("READDIR of a file: status %d, want NOTDIR", s)
	}
}

// TestV4NamespaceChangeInfo checks the change_info4 of CREATE, REMOVE and
// RENAME: atomic, from the directory's change attribute before to after.
func TestV4NamespaceChangeInfo(t *testing.T) {
	fs := newTestFS(t)
	if err := os.Mkdir(fs.host("sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	change := func(p string) uint64 { return srv.changeAt4(p) }
	// Moving the mtime past the clock's granularity makes every change
	// visible.
	tick := func() { time.Sleep(2 * time.Millisecond) }

	before := change("")
	tick()
	r := c.run(v4Root(), v4Create(typeDir, "d", "", v4Mode(0o700)), v4GetFH())
	r.skip(op4PutRootFH)
	d := r.next(op4Create, stOK)
	ci := v4getCinfo(d)
	if set, _ := getBitmap4(d); set != bitmap4(attr4Mode) {
		t.Fatalf("CREATE attrset %v", set)
	}
	if id, _ := r.next(op4GetFH, stOK).fh(8); id != fs.FileID("d") {
		t.Fatal("CREATE did not make the new directory current")
	}
	r.end()
	if !ci.atomic || ci.before != before || ci.after != change("") || ci.after == ci.before {
		t.Fatalf("CREATE cinfo %+v, directory %x then %x", ci, before, change(""))
	}
	if info, _ := os.Stat(fs.host("d")); info.Mode().Perm() != hostPerm(0o700, true) {
		t.Fatalf("mode %v", info.Mode())
	}

	v4write(t, fs, "f", "x")
	v4write(t, fs, "plain", "x")
	rootBefore, subBefore := change(""), change("sub")
	tick()
	r = c.run(v4Root(), v4Save(), v4Look("sub"), v4Rename("f", "g"))
	r.skip(op4PutRootFH, op4SaveFH, op4Lookup)
	d = r.next(op4Rename, stOK)
	src, dst := v4getCinfo(d), v4getCinfo(d)
	r.end()
	if !src.atomic || src.before != rootBefore || src.after != change("") ||
		!dst.atomic || dst.before != subBefore || dst.after != change("sub") || dst.after == dst.before {
		t.Fatalf("RENAME cinfo %+v %+v", src, dst)
	}
	if _, err := os.Stat(fs.host("sub/g")); err != nil {
		t.Fatal(err)
	}

	subBefore = change("sub")
	tick()
	r = c.run(v4Root(), v4Look("sub"), v4Remove("g"))
	r.skip(op4PutRootFH, op4Lookup)
	if ci := v4getCinfo(r.next(op4Remove, stOK)); !ci.atomic || ci.before != subBefore || ci.after != change("sub") {
		t.Fatalf("REMOVE cinfo %+v", ci)
	}
	r.end()

	for _, tc := range []struct {
		what string
		ops  []v4op
		want status
	}{
		{"REMOVE of a missing name", []v4op{v4Root(), v4Remove("missing")}, stNoEnt},
		{"REMOVE of a non-empty directory", []v4op{v4Root(), v4Create(typeDir, "full", "", v4fattr{}), v4Create(typeDir, "x", "", v4fattr{}), v4Root(), v4Remove("full")}, stNotEmpty},
		{"RENAME without a saved filehandle", []v4op{v4Root(), v4Rename("d", "e")}, stNoFileHandle},
		{"RENAME of a missing name", []v4op{v4Root(), v4Save(), v4Rename("missing", "e")}, stNoEnt},
		{"CREATE of a regular file", []v4op{v4Root(), v4Create(typeReg, "r", "", v4fattr{})}, stBadType},
		{"CREATE of a device", []v4op{v4Root(), v4Create(typeBlk, "b", "", v4fattr{})}, stNotSupp},
		{"CREATE of an empty symlink", []v4op{v4Root(), v4Create(typeLnk, "s", "", v4fattr{})}, stInval},
		{"CREATE of an unknown type", []v4op{v4Root(), v4Create(9, "u", "", v4fattr{})}, stBadType},
		{"CREATE in a file", []v4op{v4Root(), v4Look("plain"), v4Create(typeDir, "u", "", v4fattr{})}, stNotDir},
	} {
		if s := c.run(tc.ops...).status; s != tc.want {
			t.Errorf("%s: status %d, want %d", tc.what, s, tc.want)
		}
	}
}

// v4SlowMkdir takes long enough over each mkdir that concurrent creates
// would overlap if nothing serialized them.
type v4SlowMkdir struct{ *testFS }

// MkdirAll sleeps, then makes the directory.
func (fs v4SlowMkdir) MkdirAll(name string, perm os.FileMode) error {
	time.Sleep(5 * time.Millisecond)
	return fs.testFS.MkdirAll(name, perm)
}

// TestV4DirectoryChangesChainAtomically: a client keeps its cached names
// for a directory only across change info that is atomic and starts where
// its cache left off. Concurrent creates in one directory must therefore
// report intervals that chain: no two start from the same value, and
// together they lead from the change before to the change after.
func TestV4DirectoryChangesChainAtomically(t *testing.T) {
	fs := newTestFS(t)
	srv, addr := v4serve(t, v4SlowMkdir{fs}, nil)
	first := srv.changeAt4("")
	const n = 16
	conns := make([]*v4conn, n)
	for i := range conns {
		conns[i] = v4dial(t, addr)
	}
	infos := make([]v4cinfo, n)
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Go(func() {
			r := c.run(v4Root(), v4Create(typeDir, fmt.Sprintf("d%d", i), "", v4Mode(0o755)))
			if r.status != stOK {
				t.Errorf("CREATE d%d: status %d", i, r.status)
				return
			}
			r.skip(op4PutRootFH)
			infos[i] = v4getCinfo(r.next(op4Create, stOK))
		})
	}
	wg.Wait()
	next := make(map[uint64]uint64, n)
	for _, ci := range infos {
		if !ci.atomic {
			t.Fatalf("change info %+v is not atomic", ci)
		}
		if _, dup := next[ci.before]; dup {
			t.Fatalf("two creates report the same before %x: they overlapped", ci.before)
		}
		next[ci.before] = ci.after
	}
	at := first
	for range n {
		after, ok := next[at]
		if !ok {
			t.Fatalf("no create starts at %x: the intervals do not chain", at)
		}
		at = after
	}
	if last := srv.changeAt4(""); at != last {
		t.Fatalf("chain ends at %x, directory is at %x", at, last)
	}
}

// TestV4DirectoryChangeIgnoresBackingEntries: a directory's entries may be
// rearranged without a visible change (ovnfs moving a file from memory to
// disk keeps the directory's mtime); its change attribute must stay put,
// or clients purge their cached names for nothing.
func TestV4DirectoryChangeIgnoresBackingEntries(t *testing.T) {
	fs := newTestFS(t)
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	rootChange := func() uint64 {
		r := c.run(v4Root(), v4GetAttr(attr4Change))
		r.skip(op4PutRootFH)
		d := r.next(op4GetAttr, stOK)
		getBitmap4(d)
		d.u32()
		v := d.u64()
		r.end()
		return v
	}
	before := rootChange()
	info, err := os.Stat(fs.root)
	if err != nil {
		t.Fatal(err)
	}
	v4write(t, fs, "moved-to-disk", "data")
	if err := os.Chtimes(fs.root, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after := rootChange(); after != before {
		t.Fatalf("change moved from %x to %x with the mtime kept", before, after)
	}
}
