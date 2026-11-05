package nfs

import (
	"slices"
	"testing"
	"time"
)

// TestV3ChangesRecallV4Delegations checks that every NFSv3 procedure that
// changes a file recalls the NFSv4 read delegations of it: the WRITE,
// SETATTR and LINK target, the removed file, and both the moved and the
// replaced file of a RENAME. Without the recall, NFSv4 clients would keep
// serving stale cached data.
func TestV3ChangesRecallV4Delegations(t *testing.T) {
	fs, srv, addr := v4delegServer(t, nil)
	holder, cb := v4holder(t, srv, addr, "holder")
	v3 := dial(t, addr)
	for _, name := range []string{"written", "chmodded", "removed", "moved", "replaced", "linked"} {
		v4write(t, fs, name, "data")
	}
	delegate := func(name string) (v4StateID, uint64) {
		return holder.mustDelegate("o-"+name, name), fs.FileID(name)
	}
	dirop := func(e *encoder, name string) {
		e.fh(1)
		e.str(name)
	}
	type recalled struct {
		id   v4StateID
		file uint64
	}
	expect := func(what string, want ...recalled) {
		t.Helper()
		var got []recalled
		for range want {
			select {
			case r := <-cb.recalls:
				got = append(got, recalled{r.id, r.file})
			case <-time.After(5 * time.Second):
				t.Fatalf("%s: recalls %+v, want %+v", what, got, want)
			}
		}
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Fatalf("%s: recalls %+v, want %+v", what, got, want)
			}
		}
	}

	id, file := delegate("written")
	if st, _, _, _ := v3.write3(file, 0, fileSync3, []byte("x")); st != stOK {
		t.Fatalf("WRITE: status %d", st)
	}
	expect("WRITE", recalled{id, file})

	id, file = delegate("chmodded")
	if st, _ := v3.v3call(v3SetAttr, func(e *encoder) {
		e.fh(file)
		e.u32(1) // set mode
		e.u32(0o600)
		for range 5 { // uid, gid, size, atime, mtime: unchanged
			e.u32(0)
		}
		e.u32(0) // no guard
	}); st != stOK {
		t.Fatalf("SETATTR: status %d", st)
	}
	expect("SETATTR", recalled{id, file})

	id, file = delegate("removed")
	if st, _ := v3.v3call(v3Remove, func(e *encoder) { dirop(e, "removed") }); st != stOK {
		t.Fatalf("REMOVE: status %d", st)
	}
	expect("REMOVE", recalled{id, file})

	movedID, moved := delegate("moved")
	replacedID, replaced := delegate("replaced")
	if st, _ := v3.v3call(v3Rename, func(e *encoder) {
		dirop(e, "moved")
		dirop(e, "replaced")
	}); st != stOK {
		t.Fatalf("RENAME: status %d", st)
	}
	expect("RENAME", recalled{movedID, moved}, recalled{replacedID, replaced})

	id, file = delegate("linked")
	if st, _ := v3.v3call(v3Link, func(e *encoder) {
		e.fh(file)
		dirop(e, "link")
	}); st != stOK {
		t.Fatalf("LINK: status %d", st)
	}
	expect("LINK", recalled{id, file})
}
