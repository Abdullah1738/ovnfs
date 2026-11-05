package nfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
)

// TestRPCProgramAndVersionMismatch checks the replies for programs and
// versions the server does not serve.
func TestRPCProgramAndVersionMismatch(t *testing.T) {
	c := dial(t, startServer(t, &Server{FS: newTestFS(t)}))
	if stat, _ := c.call(100000, 2, 0, nil); stat != rpcProgUnavail {
		t.Fatalf("portmapper: accept_stat %d, want PROG_UNAVAIL", stat)
	}
	for _, tc := range []struct{ prog, vers, low, high uint32 }{
		{progNFS, 2, 3, 4},
		{progNFS, 5, 3, 4},
		{progMount, 1, 3, 3},
	} {
		stat, d := c.call(tc.prog, tc.vers, 0, nil)
		if low, high := d.u32(), d.u32(); stat != rpcProgMismatch || low != tc.low || high != tc.high {
			t.Errorf("program %d v%d: accept_stat %d, versions %d-%d", tc.prog, tc.vers, stat, low, high)
		}
	}
}

// TestRPCFragmentsAndDeniedCalls sends a call split across record fragments,
// then an RPC version and a credential flavor the server refuses.
func TestRPCFragmentsAndDeniedCalls(t *testing.T) {
	c := dial(t, startServer(t, &Server{FS: newTestFS(t)}))
	call := func(rpcvers, flavor uint32) []uint32 {
		body := binary.BigEndian.AppendUint32(nil, 7) // xid
		for _, v := range []uint32{msgCall, rpcvers, progNFS, 2, 0, flavor, 0, authNone, 0} {
			body = binary.BigEndian.AppendUint32(body, v)
		}
		// Two fragments: the first without the last-fragment bit.
		rec := binary.BigEndian.AppendUint32(nil, 12)
		rec = append(rec, body[:12]...)
		rec = binary.BigEndian.AppendUint32(rec, lastFragment|uint32(len(body)-12))
		rec = append(rec, body[12:]...)
		if _, err := c.nc.Write(rec); err != nil {
			t.Fatal(err)
		}
		reply, err := readRecord(c.r, nil)
		if err != nil {
			t.Fatal(err)
		}
		words := make([]uint32, len(reply)/4)
		for i := range words {
			words[i] = binary.BigEndian.Uint32(reply[4*i:])
		}
		return words
	}
	want := func(name string, got []uint32, want ...uint32) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: reply %v, want %v", name, got, want)
		}
	}
	want("fragmented", call(2, authNone), 7, msgReply, replyAccepted, authNone, 0, uint32(rpcProgMismatch), 3, 4)
	want("rpc version", call(3, authNone), 7, msgReply, replyDenied, rejectRPCMismatch, 2, 2)
	want("flavor", call(2, 6), 7, msgReply, replyDenied, rejectAuthError, authBadCred)
}

// TestRPCMalformedCallCloses checks that a record too short to be a call
// header ends the connection instead of desynchronizing it.
func TestRPCMalformedCallCloses(t *testing.T) {
	c := dial(t, startServer(t, &Server{FS: newTestFS(t)}))
	if _, err := c.nc.Write([]byte{0x80, 0, 0, 4, 0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecord(c.r, nil); err != io.EOF {
		t.Fatalf("read after malformed call: %v, want EOF", err)
	}
}

// TestStatusOfKeepsErrno checks wrapped errnos keep distinct statuses,
// EPERM is not taken for EACCES, and ENOTEMPTY not for EEXIST.
func TestStatusOfKeepsErrno(t *testing.T) {
	for err, want := range map[error]status{
		&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EXDEV}: stXDev,
		&os.PathError{Op: "rmdir", Path: "d", Err: syscall.ENOTEMPTY}:       stNotEmpty,
		fmt.Errorf("wrapped: %w", syscall.EEXIST):                           stExist,
		syscall.EPERM:       stPerm,
		syscall.EACCES:      stAccess,
		syscall.ENOENT:      stNoEnt,
		syscall.ENOTSUP:     stNotSupp,
		syscall.EDQUOT:      stDQuot,
		io.ErrUnexpectedEOF: stIO,
	} {
		if got := statusOf(err); got != want {
			t.Errorf("statusOf(%v) = %d, want %d", err, got, want)
		}
	}
}

// TestListingContinuation checks that continuations page through the
// listing they started on, and that a continuation whose snapshot is gone
// survives only an unchanged directory.
func TestListingContinuation(t *testing.T) {
	fs := newTestFS(t)
	s := &Server{FS: fs}
	for _, name := range []string{"b", "a", "c"} {
		if err := os.WriteFile(fs.host(name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first, st := s.list(1, "", 0, 0)
	if st != stOK || len(first.entries) != 3 || first.entries[0].Name() != "a" {
		t.Fatalf("first listing: %v %v", st, first)
	}
	if err := os.Remove(fs.host("a")); err != nil {
		t.Fatal(err)
	}
	next, st := s.list(1, "", firstCookie, first.verf)
	if st != stOK || next != first {
		t.Fatalf("continuation did not reuse its snapshot: %v", st)
	}
	if i, ok := next.after(firstCookie); !ok || next.entries[i].Name() != "b" {
		t.Fatalf("after first cookie: %d %v", i, ok)
	}
	if _, ok := next.after(firstCookie + 3); ok {
		t.Fatal("cookie past the end accepted")
	}

	s.dirs = dirCache{} // the snapshot is evicted
	if _, st := s.list(1, "", firstCookie, first.verf); st != stBadCookie {
		t.Fatalf("changed directory continued with %v", st)
	}
	fresh, _ := s.list(1, "", 0, 0)
	s.dirs = dirCache{}
	if again, st := s.list(1, "", firstCookie, fresh.verf); st != stOK || again.verf != fresh.verf {
		t.Fatalf("unchanged directory did not continue: %v", st)
	}
}
