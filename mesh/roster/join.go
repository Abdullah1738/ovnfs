package roster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/stencil-hq/ovnfs/mesh"
)

// ALPN is the membership protocol: joining with a ticket, and members syncing
// rosters. Any node may connect on it; only a ticket or membership gets past
// the first message.
const ALPN = "o2-mesh/1"

// joinVersion is the exchange this build speaks.
const joinVersion = 1

// maxMessage bounds one message; a roster of a few hundred members fits.
const maxMessage = 1 << 20

// denied is the one refusal a joiner hears, whatever its cause.
const denied = "denied"

// message is one frame of the exchange, JSON in a [mesh.WriteFrame] frame.
type message struct {
	T      string  `json:"t"` // "join", "welcome", "sync" or "denied"
	V      int     `json:"v,omitempty"`
	Mesh   string  `json:"mesh,omitempty"`
	Secret []byte  `json:"secret,omitempty"` // join: the ticket secret
	Member *Member `json:"member,omitempty"` // join: the joiner's name and ways; its node comes from TLS
	State  *State  `json:"state,omitempty"`  // welcome, sync: a roster
	Reason string  `json:"reason,omitempty"` // denied
}

// send writes one message.
func send(w io.Writer, m message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return mesh.WriteFrame(w, data)
}

// receive reads one message.
func receive(r io.Reader) (message, error) {
	data, err := mesh.ReadFrame(r, maxMessage)
	if err != nil {
		return message{}, err
	}
	var m message
	if err := json.Unmarshal(data, &m); err != nil {
		return message{}, fmt.Errorf("mesh message: %w", err)
	}
	return m, nil
}

// ErrDenied is a refusal by the other side.
var ErrDenied = errors.New("denied")

// Join redeems tok's ticket with the inviter on rw, a stream of a connection to
// tok.Node (whose key the connection pinned), and returns the mesh's roster,
// this machine included.
func Join(rw io.ReadWriter, tok Token, name string, ways []mesh.Way) (State, error) {
	self := Member{Name: name, Ways: clipWays(ways)}
	if err := send(rw, message{T: "join", V: joinVersion, Mesh: tok.Mesh, Secret: tok.Secret[:], Member: &self}); err != nil {
		return State{}, err
	}
	m, err := receive(rw)
	if err != nil {
		return State{}, err
	}
	switch {
	case m.T == "denied" && m.Reason == denied:
		return State{}, fmt.Errorf("the inviter %w the ticket: tickets are single-use and expire after %s; mint a new one with `ovnfs mesh setup`", ErrDenied, TicketTTL)
	case m.T == "denied":
		return State{}, fmt.Errorf("the inviter %w the join: %s", ErrDenied, m.Reason)
	case m.T != "welcome" || m.State == nil || m.State.Mesh != tok.Mesh:
		return State{}, fmt.Errorf("unexpected %q answer to a join", m.T)
	}
	return *m.State, nil
}

// Sync sends this member's roster on rw and returns the other member's.
func Sync(rw io.ReadWriter, st State) (State, error) {
	if err := send(rw, message{T: "sync", Mesh: st.Mesh, State: &st}); err != nil {
		return State{}, err
	}
	m, err := receive(rw)
	if err != nil {
		return State{}, err
	}
	switch {
	case m.T == "denied":
		return State{}, fmt.Errorf("%w: %s", ErrDenied, m.Reason)
	case m.T != "sync" || m.State == nil:
		return State{}, fmt.Errorf("unexpected %q answer to a sync", m.T)
	}
	return *m.State, nil
}

// Outcome is what one served exchange changed.
type Outcome struct {
	Joined  *Member // a new member redeemed a ticket
	Changed bool    // the roster changed (a join, or a sync brought news)
}

// Serve answers one exchange on rw from peer, the node the connection's TLS
// handshake proved: a join with a live ticket admits it; a sync from a member
// merges its roster and answers with this one. Anything else is denied.
func (b *Book) Serve(rw io.ReadWriter, peer mesh.NodeID) (Outcome, error) {
	m, err := receive(rw)
	if err != nil {
		return Outcome{}, err
	}
	refuse := func(reason string, cause error) (Outcome, error) {
		if err := send(rw, message{T: "denied", Reason: reason}); err != nil {
			return Outcome{}, err
		}
		return Outcome{}, cause
	}
	st := b.State()
	switch m.T {
	case "join":
		switch {
		case m.V != joinVersion:
			return refuse(fmt.Sprintf("join version %d", joinVersion), fmt.Errorf("join version %d from %s", m.V, peer.Short()))
		case st.Mesh == "" || m.Mesh != st.Mesh:
			return refuse(denied, fmt.Errorf("join of mesh %q from %s", m.Mesh, peer.Short()))
		case peer == b.self:
			return refuse(denied, errors.New("join from this machine"))
		case !b.redeem(m.Secret, time.Now()):
			return refuse(denied, fmt.Errorf("join from %s with a spent or unknown ticket", peer.Short()))
		}
		joined := Member{Node: peer}
		if m.Member != nil {
			joined.Name, joined.Ways = m.Member.Name, m.Member.Ways
		}
		if err := b.admit(joined); err != nil {
			return Outcome{}, err
		}
		st = b.State()
		if err := send(rw, message{T: "welcome", State: &st}); err != nil {
			return Outcome{}, err
		}
		return Outcome{Joined: &joined, Changed: true}, nil
	case "sync":
		if !b.IsMember(peer) || m.State == nil {
			return refuse(denied, fmt.Errorf("sync from non-member %s", peer.Short()))
		}
		changed, err := b.Merge(*m.State)
		if err != nil {
			return refuse(denied, err)
		}
		st = b.State()
		if err := send(rw, message{T: "sync", State: &st}); err != nil {
			return Outcome{}, err
		}
		return Outcome{Changed: changed}, nil
	}
	return refuse(denied, fmt.Errorf("unknown mesh message %q from %s", m.T, peer.Short()))
}
