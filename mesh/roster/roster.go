// Package roster keeps who is in this machine's mesh: the members (pinned node
// keys and where each last said it listens), the single-use invite tickets a new
// machine joins with, the join token that carries one, and the join and sync
// exchanges members run on [ALPN].
//
// Membership is the set of member keys; there is no trust on first use inside
// the mesh. Every member keeps the whole roster and members reconcile it by
// syncing: each entry is a last-writer-wins register per node, a removal being a
// tombstone entry, so any member's view converges on the newest write.
//
// # Example
//
//	b, _ := roster.Open(dir, key.ID())
//	st, _ := b.Create("")                       // `ovnfs mesh setup` on the first machine
//	secret, expires, _ := b.Invite(time.Now())
//	tok := roster.Token{Mesh: st.Mesh, Node: key.ID(), Ways: ways, Secret: secret}
//	fmt.Println(tok, "valid until", expires)    // paste into `ovnfs mesh join` elsewhere
package roster

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/mesh"
)

const (
	// File holds the roster in the mesh state directory.
	File = "roster.json"
	// invitesFile holds the live tickets' hashes, never the secrets.
	invitesFile = "invites.json"
	// TicketTTL is how long an invite ticket can be redeemed.
	TicketTTL = 10 * time.Minute
	// maxInvites caps live tickets; minting more drops the oldest.
	maxInvites = 16
	// maxWays caps the ways one member advertises.
	maxWays = 8
)

// Member is one machine of the mesh, known by its node key.
type Member struct {
	Node    mesh.NodeID `json:"node"`
	Name    string      `json:"name"`              // its hostname, for people and `attach --to`
	Ways    []mesh.Way  `json:"ways,omitempty"`    // where it last said it listens, most preferred first
	Updated time.Time   `json:"updated"`           // when the entry was written; the newer entry wins a merge
	Removed bool        `json:"removed,omitempty"` // a tombstone: the node left or was removed at Updated
}

// State is a mesh as one member knows it.
type State struct {
	Mesh    string   `json:"mesh"`            // random id naming the mesh; "" = not in one
	Relay   string   `json:"relay,omitempty"` // relay every member uses; "" = the production relay
	Members []Member `json:"members"`         // sorted by node, tombstones included
}

// Live returns the members that have not been removed.
func (s State) Live() []Member {
	var live []Member
	for _, m := range s.Members {
		if !m.Removed {
			live = append(live, m)
		}
	}
	return live
}

// invite is one live ticket: the SHA-256 of its secret and its expiry.
type invite struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

// Book is this machine's roster and tickets, persisted in one directory; safe
// for concurrent use within one process (the daemon).
type Book struct {
	dir     string
	self    mesh.NodeID
	mu      sync.Mutex
	st      State
	invites []invite
}

// Open loads the roster kept in dir, creating dir (private) if needed.
func Open(dir string, self mesh.NodeID) (*Book, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b := &Book{dir: dir, self: self}
	if err := load(filepath.Join(dir, File), &b.st); err != nil {
		return nil, fmt.Errorf("roster: %w", err)
	}
	if err := load(filepath.Join(dir, invitesFile), &b.invites); err != nil {
		return nil, fmt.Errorf("roster invites: %w", err)
	}
	return b, nil
}

// load decodes a JSON file into v; a missing file leaves v as it is.
func load(name string, v any) error {
	data, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// save writes v as JSON to name through a private temp file and a rename.
func save(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(append(data, '\n')); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// Self is this machine's node id.
func (b *Book) Self() mesh.NodeID { return b.self }

// State returns a copy of the roster.
func (b *Book) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.copyState()
}

// copyState deep-copies the roster; the caller holds mu.
func (b *Book) copyState() State {
	st := b.st
	st.Members = slices.Clone(b.st.Members)
	for i := range st.Members {
		st.Members[i].Ways = slices.Clone(st.Members[i].Ways)
	}
	return st
}

// Create starts a mesh with this machine as its only member, unless it is in
// one already, and returns the roster. relay is the relay members will use
// ("" = production).
func (b *Book) Create(relay string) (State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Mesh != "" {
		return b.copyState(), nil
	}
	var id [8]byte
	rand.Read(id[:])
	b.st = State{Mesh: hex.EncodeToString(id[:]), Relay: relay}
	return b.copyState(), b.saveState()
}

// saveState persists the roster; the caller holds mu.
func (b *Book) saveState() error { return save(filepath.Join(b.dir, File), b.st) }

// SetSelf records this machine's name and ways in its own entry and reports
// whether that changed anything; outside a mesh it does nothing.
func (b *Book) SetSelf(name string, ways []mesh.Way) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Mesh == "" {
		return false, nil
	}
	ways = clipWays(ways)
	if i, ok := b.find(b.self); ok {
		m := b.st.Members[i]
		if !m.Removed && m.Name == name && slices.Equal(m.Ways, ways) {
			return false, nil
		}
	}
	b.put(Member{Node: b.self, Name: name, Ways: ways, Updated: b.stamp()})
	return true, b.saveState()
}

// stamp is a write time later than every entry's, so a fresh write always wins
// even when clocks disagree a little; the caller holds mu.
func (b *Book) stamp() time.Time {
	now := time.Now().UTC().Round(0)
	for _, m := range b.st.Members {
		if !now.After(m.Updated) {
			now = m.Updated.Add(time.Millisecond)
		}
	}
	return now
}

// find locates node's entry; the caller holds mu.
func (b *Book) find(node mesh.NodeID) (int, bool) {
	return slices.BinarySearchFunc(b.st.Members, node, func(m Member, id mesh.NodeID) int {
		return bytes.Compare(m.Node[:], id[:])
	})
}

// put inserts or replaces m's entry, keeping members sorted; the caller holds mu.
func (b *Book) put(m Member) {
	if i, ok := b.find(m.Node); ok {
		b.st.Members[i] = m
	} else {
		b.st.Members = slices.Insert(b.st.Members, i, m)
	}
}

// IsMember reports whether node is a live member (this machine included).
func (b *Book) IsMember(node mesh.NodeID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	i, ok := b.find(node)
	return ok && b.st.Mesh != "" && !b.st.Members[i].Removed
}

// Ways is where to dial a member, most preferred first; it serves as the
// mesh node's peer book.
func (b *Book) Ways(node mesh.NodeID) []mesh.Way {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i, ok := b.find(node); ok && !b.st.Members[i].Removed {
		return slices.Clone(b.st.Members[i].Ways)
	}
	return nil
}

// Find resolves a member other than this machine by name, by a hex prefix of
// its node id (at least 4 digits) or by its full id.
func (b *Book) Find(q string) (Member, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var hits []Member
	for _, m := range b.st.Members {
		if !m.Removed && m.Node != b.self && strings.EqualFold(m.Name, q) {
			hits = append(hits, m)
		}
	}
	if len(hits) == 0 {
		if id, err := mesh.ParseNodeID(q); err == nil {
			q = id.String()
		}
		if len(q) >= 4 {
			lower := strings.ToLower(q)
			for _, m := range b.st.Members {
				if !m.Removed && m.Node != b.self && strings.HasPrefix(m.Node.String(), lower) {
					hits = append(hits, m)
				}
			}
		}
	}
	switch len(hits) {
	case 0:
		return Member{}, fmt.Errorf("%q is not a member of this mesh", q)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, m := range hits {
		ids[i] = m.Node.Short()
	}
	return Member{}, fmt.Errorf("%q names %d members (%s); use a node id", q, len(hits), strings.Join(ids, ", "))
}

// admit adds (or refreshes) a member that redeemed a ticket.
func (b *Book) admit(m Member) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	m.Ways = clipWays(m.Ways)
	m.Removed = false
	m.Updated = b.stamp()
	b.put(m)
	return b.saveState()
}

// Merge folds another member's roster of the same mesh into this one, entry by
// entry, the newer write winning, and reports whether anything changed. Only
// this machine writes its own entry, except for a tombstone: a roster saying
// this machine was removed makes it leave.
func (b *Book) Merge(other State) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Mesh == "" || other.Mesh != b.st.Mesh {
		return false, fmt.Errorf("roster of mesh %q does not belong to mesh %q", other.Mesh, b.st.Mesh)
	}
	changed := false
	for _, m := range other.Members {
		i, ok := b.find(m.Node)
		if ok && !m.Updated.After(b.st.Members[i].Updated) {
			continue
		}
		if m.Node == b.self {
			if m.Removed {
				b.st = State{}
				return true, b.saveState()
			}
			continue
		}
		m.Ways = clipWays(m.Ways)
		b.put(m)
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, b.saveState()
}

// Adopt takes the roster an inviter answered a join with. A machine already in
// another mesh with other live members refuses; it has to leave first.
func (b *Book) Adopt(st State) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Mesh != "" && b.st.Mesh != st.Mesh {
		for _, m := range b.st.Members {
			if !m.Removed && m.Node != b.self {
				return fmt.Errorf("this machine is in mesh %s with other members; run `ovnfs mesh leave` first", b.st.Mesh)
			}
		}
	}
	b.st = State{Mesh: st.Mesh, Relay: st.Relay}
	for _, m := range st.Members {
		m.Ways = clipWays(m.Ways)
		b.put(m)
	}
	b.invites = nil
	if err := save(filepath.Join(b.dir, invitesFile), b.invites); err != nil {
		return err
	}
	return b.saveState()
}

// Remove tombstones a member other than this machine and returns the roster to
// push to the others.
func (b *Book) Remove(node mesh.NodeID) (State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if node == b.self {
		return State{}, errors.New("use leave to take this machine out of the mesh")
	}
	i, ok := b.find(node)
	if !ok || b.st.Members[i].Removed {
		return State{}, fmt.Errorf("%s is not a member", node.Short())
	}
	m := b.st.Members[i]
	b.put(Member{Node: m.Node, Name: m.Name, Updated: b.stamp(), Removed: true})
	return b.copyState(), b.saveState()
}

// Leave takes this machine out of its mesh: it forgets the roster and its
// tickets and returns the roster with its own tombstone, for the members still
// reachable to learn it left.
func (b *Book) Leave() (State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Mesh == "" {
		return State{}, errors.New("this machine is not in a mesh")
	}
	name := ""
	if i, ok := b.find(b.self); ok {
		name = b.st.Members[i].Name
	}
	b.put(Member{Node: b.self, Name: name, Updated: b.stamp(), Removed: true})
	last := b.copyState()
	b.st = State{}
	b.invites = nil
	if err := save(filepath.Join(b.dir, invitesFile), b.invites); err != nil {
		return State{}, err
	}
	return last, b.saveState()
}

// Invite mints a ticket: a random secret only its holder knows, redeemable once
// until it expires. Only the secret's hash is kept.
func (b *Book) Invite(now time.Time) ([32]byte, time.Time, error) {
	var secret [32]byte
	rand.Read(secret[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Mesh == "" {
		return secret, time.Time{}, errors.New("this machine is not in a mesh")
	}
	live := b.liveInvites(now)
	if len(live) >= maxInvites {
		live = live[len(live)-maxInvites+1:]
	}
	sum := sha256.Sum256(secret[:])
	expires := now.Add(TicketTTL).UTC().Round(0)
	b.invites = append(live, invite{Hash: hex.EncodeToString(sum[:]), Expires: expires})
	return secret, expires, save(filepath.Join(b.dir, invitesFile), b.invites)
}

// liveInvites drops expired tickets; the caller holds mu.
func (b *Book) liveInvites(now time.Time) []invite {
	return slices.DeleteFunc(b.invites, func(in invite) bool { return !now.Before(in.Expires) })
}

// redeem burns the ticket secret belongs to, reporting whether it was live.
func (b *Book) redeem(secret []byte, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	sum := sha256.Sum256(secret)
	b.invites = b.liveInvites(now)
	hit := -1
	for i, in := range b.invites {
		want, err := hex.DecodeString(in.Hash)
		if err == nil && subtle.ConstantTimeCompare(want, sum[:]) == 1 {
			hit = i
		}
	}
	if hit < 0 {
		return false
	}
	b.invites = slices.Delete(b.invites, hit, hit+1)
	return save(filepath.Join(b.dir, invitesFile), b.invites) == nil
}

// clipWays bounds what one member may advertise, keeping its relay way (if
// any) last when direct ways have to go.
func clipWays(ways []mesh.Way) []mesh.Way {
	direct := slices.DeleteFunc(slices.Clone(ways), func(w mesh.Way) bool { return w.Relay })
	relayed := len(direct) < len(ways)
	limit := maxWays
	if relayed {
		limit--
	}
	direct = direct[:min(len(direct), limit)]
	if relayed {
		direct = append(direct, mesh.Way{Relay: true})
	}
	return direct
}
