package roster

import (
	"crypto/ed25519"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/mesh"
)

// newID returns a fresh node id.
func newID(t *testing.T) mesh.NodeID {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return mesh.NodeID(pub)
}

// newBook opens an empty roster for a fresh node.
func newBook(t *testing.T) *Book {
	t.Helper()
	b, err := Open(t.TempDir(), newID(t))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serve runs one exchange on a pipe: b serves peer while client drives the other end.
func serve[T any](t *testing.T, b *Book, peer mesh.NodeID, client func(net.Conn) (T, error)) (T, Outcome, error) {
	t.Helper()
	near, far := net.Pipe()
	defer near.Close()
	done := make(chan Outcome, 1)
	go func() {
		defer far.Close()
		out, _ := b.Serve(far, peer)
		done <- out
	}()
	got, err := client(near)
	return got, <-done, err
}

// found starts a mesh on b and returns a token for one more machine.
func found(t *testing.T, b *Book) Token {
	t.Helper()
	st, err := b.Create("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetSelf("a", []mesh.Way{{Host: "10.0.0.1", Port: 8377}}); err != nil {
		t.Fatal(err)
	}
	secret, _, err := b.Invite(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return Token{Mesh: st.Mesh, Node: b.Self(), Ways: []mesh.Way{{Host: "10.0.0.1", Port: 8377}}, Secret: secret}
}

// join redeems tok from joiner against inviter and adopts the answer.
func join(t *testing.T, inviter, joiner *Book, tok Token, name string) error {
	t.Helper()
	st, _, err := serve(t, inviter, joiner.Self(), func(c net.Conn) (State, error) {
		return Join(c, tok, name, []mesh.Way{{Relay: true}})
	})
	if err != nil {
		return err
	}
	return joiner.Adopt(st)
}

// TestTicketAdmitsOnceAndPinsBothWays checks a join makes each side a member of
// the other's roster and that the ticket cannot be redeemed again.
func TestTicketAdmitsOnceAndPinsBothWays(t *testing.T) {
	a, b, c := newBook(t), newBook(t), newBook(t)
	tok := found(t, a)
	if err := join(t, a, b, tok, "b"); err != nil {
		t.Fatal(err)
	}
	if !a.IsMember(b.Self()) || !b.IsMember(a.Self()) || !b.IsMember(b.Self()) {
		t.Fatalf("after join: a knows b %v, b knows a %v, b knows itself %v", a.IsMember(b.Self()), b.IsMember(a.Self()), b.IsMember(b.Self()))
	}
	if got := b.Ways(a.Self()); len(got) != 1 || got[0].Host != "10.0.0.1" {
		t.Fatalf("b's ways for a = %v", got)
	}
	if got := a.Ways(b.Self()); len(got) != 1 || !got[0].Relay {
		t.Fatalf("a's ways for b = %v", got)
	}
	if err := join(t, a, c, tok, "c"); !errors.Is(err, ErrDenied) {
		t.Fatalf("second redemption: err = %v, want denied", err)
	}
	if a.IsMember(c.Self()) {
		t.Fatal("a spent ticket admitted c")
	}
}

// TestTicketExpiresAndSurvivesRestart checks tickets persist across a reopen
// and stop working after TicketTTL.
func TestTicketExpiresAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir, newID(t))
	if err != nil {
		t.Fatal(err)
	}
	tok := found(t, a)
	reopened, err := Open(dir, a.Self())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.redeem(tok.Secret[:], time.Now().Add(TicketTTL)) {
		t.Fatal("an expired ticket was redeemed")
	}
	secret, _, err := reopened.Invite(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, a.Self())
	if err != nil {
		t.Fatal(err)
	}
	if !again.redeem(secret[:], time.Now()) {
		t.Fatal("a live ticket did not survive a restart")
	}
}

// TestWrongMeshAndNonMemberAreDenied checks the two refusals of the exchange.
func TestWrongMeshAndNonMemberAreDenied(t *testing.T) {
	a, b := newBook(t), newBook(t)
	tok := found(t, a)
	tok.Mesh = "0000000000000000"
	if err := join(t, a, b, tok, "b"); !errors.Is(err, ErrDenied) {
		t.Fatalf("join of another mesh: err = %v, want denied", err)
	}
	_, out, err := serve(t, a, b.Self(), func(c net.Conn) (State, error) { return Sync(c, a.State()) })
	if !errors.Is(err, ErrDenied) || out.Changed {
		t.Fatalf("sync from a non-member: err = %v, changed = %v", err, out.Changed)
	}
}

// TestRemovalSpreadsAndSticks checks a removal reaches other members by sync,
// a stale roster cannot bring the removed member back, and the removed machine
// leaves when it learns it.
func TestRemovalSpreadsAndSticks(t *testing.T) {
	a, b, c := newBook(t), newBook(t), newBook(t)
	if err := join(t, a, b, found(t, a), "b"); err != nil {
		t.Fatal(err)
	}
	secret, _, err := a.Invite(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tok := Token{Mesh: a.State().Mesh, Node: a.Self(), Secret: secret}
	if err := join(t, a, c, tok, "c"); err != nil {
		t.Fatal(err)
	}
	// b learns about c from a.
	theirs, _, err := serve(t, a, b.Self(), func(conn net.Conn) (State, error) { return Sync(conn, b.State()) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Merge(theirs); err != nil || !b.IsMember(c.Self()) {
		t.Fatalf("b after sync: member c = %v, err = %v", b.IsMember(c.Self()), err)
	}
	stale := c.State()
	removed, err := a.Remove(c.Self())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Merge(removed); err != nil || b.IsMember(c.Self()) {
		t.Fatalf("b after the removal: member c = %v, err = %v", b.IsMember(c.Self()), err)
	}
	if changed, err := b.Merge(stale); err != nil || changed || b.IsMember(c.Self()) {
		t.Fatalf("stale roster: changed = %v, member c = %v, err = %v", changed, b.IsMember(c.Self()), err)
	}
	if _, err := c.Merge(removed); err != nil || c.State().Mesh != "" {
		t.Fatalf("removed machine still in mesh %q (err %v)", c.State().Mesh, err)
	}
}

// TestLeaveThenRejoin checks a machine that left can join again with a new
// ticket and is a member again everywhere the newer entry reaches.
func TestLeaveThenRejoin(t *testing.T) {
	a, b := newBook(t), newBook(t)
	if err := join(t, a, b, found(t, a), "b"); err != nil {
		t.Fatal(err)
	}
	last, err := b.Leave()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Merge(last); err != nil || a.IsMember(b.Self()) {
		t.Fatalf("after leave: a still has b = %v (err %v)", a.IsMember(b.Self()), err)
	}
	secret, _, err := a.Invite(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := join(t, a, b, Token{Mesh: a.State().Mesh, Node: a.Self(), Secret: secret}, "b"); err != nil {
		t.Fatal(err)
	}
	if !a.IsMember(b.Self()) || !b.IsMember(a.Self()) {
		t.Fatal("rejoin did not restore membership")
	}
}

// TestAdoptRefusesAnotherPopulatedMesh checks joining cannot silently drop a
// mesh that has other members.
func TestAdoptRefusesAnotherPopulatedMesh(t *testing.T) {
	a, b, x := newBook(t), newBook(t), newBook(t)
	if err := join(t, a, b, found(t, a), "b"); err != nil {
		t.Fatal(err)
	}
	if err := b.Adopt(State{Mesh: "1111111111111111", Members: []Member{{Node: x.Self(), Name: "x"}}}); err == nil {
		t.Fatal("adopted another mesh while in one with members")
	}
	solo := newBook(t)
	found(t, solo) // a mesh of one may be replaced
	if err := join(t, a, solo, func() Token {
		s, _, _ := a.Invite(time.Now())
		return Token{Mesh: a.State().Mesh, Node: a.Self(), Secret: s}
	}(), "solo"); err != nil {
		t.Fatalf("a machine alone in its mesh could not join another: %v", err)
	}
}

// TestTokenRoundTripAndRejects checks the printed token parses back and bad ones fail.
func TestTokenRoundTripAndRejects(t *testing.T) {
	tok := Token{Mesh: "0123456789abcdef", Node: newID(t), Relay: "http://127.0.0.1:7475",
		Ways: []mesh.Way{{Host: "192.168.1.2", Port: 8377}, {Host: "fd7a:115c:a1e0::1", Port: 8377}}}
	tok.Secret[0], tok.Secret[31] = 1, 2
	got, err := ParseToken("  " + tok.String() + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.Mesh != tok.Mesh || got.Node != tok.Node || got.Relay != tok.Relay || got.Secret != tok.Secret || len(got.Ways) != 2 || got.Ways[1] != tok.Ways[1] {
		t.Fatalf("round trip: %+v, want %+v", got, tok)
	}
	s := tok.String()
	for name, bad := range map[string]string{
		"prefix":    strings.Replace(s, "o2://join?", "o2://other?", 1),
		"secret":    s[:len(s)-2],
		"twice":     s + "&mesh=0123456789abcdef",
		"mesh":      strings.Replace(s, "mesh=0123456789abcdef", "mesh=xyz", 1),
		"way":       strings.Replace(s, "udp:192.168.1.2:8377", "tcp:192.168.1.2:8377", 1),
		"relay URL": strings.Replace(s, "http%3A", "ftp%3A", 1),
	} {
		if _, err := ParseToken(bad); err == nil {
			t.Errorf("%s: parsed %q", name, bad)
		}
	}
}

// TestFindByNameAndPrefix checks member lookup for `attach --to`.
func TestFindByNameAndPrefix(t *testing.T) {
	a, b, c := newBook(t), newBook(t), newBook(t)
	if err := join(t, a, b, found(t, a), "cherry"); err != nil {
		t.Fatal(err)
	}
	secret, _, _ := a.Invite(time.Now())
	if err := join(t, a, c, Token{Mesh: a.State().Mesh, Node: a.Self(), Secret: secret}, "cherry"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Find("cherry"); err == nil || !strings.Contains(err.Error(), "2 members") {
		t.Fatalf("ambiguous name: err = %v", err)
	}
	if m, err := a.Find(b.Self().String()[:8]); err != nil || m.Node != b.Self() {
		t.Fatalf("by prefix: %v, %v", m.Node.Short(), err)
	}
	if _, err := a.Find("a"); err == nil {
		t.Fatal("found this machine itself")
	}
	if _, err := a.Find("nobody"); err == nil {
		t.Fatal("found a non-member")
	}
}
