package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/mesh"
	"github.com/stencil-hq/ovnfs/mesh/node"
	"github.com/stencil-hq/ovnfs/mesh/relay"
	"github.com/stencil-hq/ovnfs/mesh/roster"
)

// linkALPN carries attach links between mesh members, one stream per attached checkout.
const linkALPN = "ovnfs-link/1"

// meshDir holds the node key and the roster, under the daemon state.
const meshDir = "mesh"

// syncTimeout bounds one roster exchange with one member.
const syncTimeout = 15 * time.Second

// meshd is the daemon's mesh: its node key, the roster and, while this machine
// is in a mesh, the node serving membership and attach links.
type meshd struct {
	key   *mesh.NodeKey
	book  *roster.Book
	links *links
	name  string // this machine's name in the roster

	mu       sync.Mutex
	node     *node.Node
	relay    *relay.Client
	relayURL string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// openMesh loads the node key and roster kept in the state directory; resume
// starts the node once the daemon can serve links.
func openMesh(state string, lk *links) (*meshd, error) {
	dir := filepath.Join(state, meshDir)
	key, err := mesh.LoadOrCreateKey(dir)
	if err != nil {
		return nil, fmt.Errorf("node key: %w", err)
	}
	book, err := roster.Open(dir, key.ID())
	if err != nil {
		return nil, err
	}
	return &meshd{key: key, book: book, links: lk, name: machineName()}, nil
}

// machineName is the host name's first label, how members name this machine.
func machineName() string {
	host, _ := os.Hostname()
	if name, _, _ := strings.Cut(host, "."); name != "" {
		return name
	}
	return "machine"
}

// relayFor resolves a mesh's relay setting to a URL; "" means none.
func relayFor(setting string) string {
	switch setting {
	case "":
		return relay.Production
	case "off":
		return ""
	}
	return setting
}

// resume starts the node if this machine is in a mesh; a failure leaves the
// daemon serving without it (mesh verbs report why).
func (m *meshd) resume() {
	if st := m.book.State(); st.Mesh != "" {
		if err := m.start(st.Relay); err != nil {
			log.Printf("mesh: %v", err)
		}
	}
}

// start brings the node up with the given relay setting; it is a no-op while
// the node runs.
func (m *meshd) start(relaySetting string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.node != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	target := relayFor(relaySetting)
	var rc *relay.Client
	if target != "" {
		rc = relay.New(target, m.key)
		m.wg.Go(func() { rc.Run(ctx) })
	}
	n, err := node.Start(ctx, node.Config{Key: m.key, Relay: rc, Book: m.book, Admit: m.admit, ALPNs: []string{roster.ALPN, linkALPN}})
	if err != nil {
		cancel()
		m.wg.Wait()
		return fmt.Errorf("mesh node: %w", err)
	}
	members, err := n.Listen(roster.ALPN)
	if err == nil {
		var attaches *node.Listener
		if attaches, err = n.Listen(linkALPN); err == nil {
			m.wg.Go(func() { m.serveMembership(ctx, members) })
			m.wg.Go(func() { m.serveLinks(ctx, attaches) })
		}
	}
	if err != nil {
		n.Close()
		cancel()
		m.wg.Wait()
		return fmt.Errorf("mesh listen: %w", err)
	}
	m.node, m.relay, m.relayURL, m.cancel = n, rc, target, cancel
	m.wg.Go(func() { m.announce(ctx, n) })
	log.Printf("mesh: node %s up (relay %s)", m.key.ID().Short(), orDefault(target, "off"))
	return nil
}

// stop takes the node down and waits for everything it served.
func (m *meshd) stop() {
	m.mu.Lock()
	n, cancel := m.node, m.cancel
	m.node, m.relay, m.relayURL, m.cancel = nil, nil, "", nil
	m.mu.Unlock()
	if n == nil {
		return
	}
	cancel()
	n.Close()
	m.wg.Wait()
}

// current returns the running node.
func (m *meshd) current() (*node.Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.node == nil {
		if m.book.State().Mesh == "" {
			return nil, &checkout.Error{Code: "unavailable", Message: "this machine is in no mesh; run `ovnfs mesh setup` or `ovnfs mesh join`"}
		}
		return nil, &checkout.Error{Code: "unavailable", Message: "the mesh node is not running; see the daemon log"}
	}
	return m.node, nil
}

// admit lets anyone reach the membership protocol (a ticket or membership is
// checked there) and only members reach anything else.
func (m *meshd) admit(p node.PeerInfo, alpn string) bool {
	return alpn == roster.ALPN || m.book.IsMember(p.Node)
}

// serveMembership answers joins and roster syncs, one exchange per stream.
func (m *meshd) serveMembership(ctx context.Context, ln *node.Listener) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		m.wg.Go(func() {
			for {
				s, err := conn.AcceptStream(ctx)
				if err != nil {
					return
				}
				m.wg.Go(func() { m.exchange(conn.Peer().Node, s) })
			}
		})
	}
}

// exchange serves one membership stream from peer and spreads what it changed.
func (m *meshd) exchange(peer mesh.NodeID, s *quic.Stream) {
	c := streamConn{s}
	defer c.Close()
	s.SetDeadline(time.Now().Add(syncTimeout))
	out, err := m.book.Serve(c, peer)
	if err != nil {
		log.Printf("mesh: %v", err)
	}
	if out.Joined != nil {
		log.Printf("mesh: %s (%s) joined", out.Joined.Name, peer.Short())
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		m.syncAll(ctx, m.book.State(), peer)
		cancel()
	}
	if out.Changed && m.book.State().Mesh == "" {
		log.Printf("mesh: this machine was removed from the mesh by %s", peer.Short())
		go m.stop()
	}
}

// serveLinks hands each attach stream a member opens to the link manager,
// naming the member the connection proved.
func (m *meshd) serveLinks(ctx context.Context, ln *node.Listener) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		m.wg.Go(func() {
			for {
				s, err := conn.AcceptStream(ctx)
				if err != nil {
					return
				}
				m.wg.Go(func() { m.links.inbound(streamConn{s}, conn.Peer().Node.String()) })
			}
		})
	}
}

// announce keeps this machine's entry current (interfaces and the relay's view
// of its address change) and tells the members when it changes; the first
// round also learns whatever changed while this daemon was down.
func (m *meshd) announce(ctx context.Context, n *node.Node) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for first := true; ; first = false {
		changed, err := m.book.SetSelf(m.name, n.Ways())
		if err != nil {
			log.Printf("mesh: %v", err)
		}
		if changed || first {
			sctx, cancel := context.WithTimeout(ctx, syncTimeout)
			m.syncAll(sctx, m.book.State())
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// syncAll exchanges st with every live member of it but this machine and
// skip, concurrently, and merges what they answer.
func (m *meshd) syncAll(ctx context.Context, st roster.State, skip ...mesh.NodeID) {
	var wg sync.WaitGroup
	for _, mem := range st.Live() {
		if mem.Node == m.key.ID() || slices.Contains(skip, mem.Node) {
			continue
		}
		wg.Go(func() {
			if err := m.sync(ctx, mem.Node, st); err != nil {
				log.Printf("mesh: sync with %s (%s): %v", mem.Name, mem.Node.Short(), err)
			}
		})
	}
	wg.Wait()
}

// sync sends st to one member and merges its answer while this machine is
// still in the mesh.
func (m *meshd) sync(ctx context.Context, id mesh.NodeID, st roster.State) error {
	n, err := m.current()
	if err != nil {
		return err
	}
	conn, err := n.Connect(ctx, id, roster.ALPN)
	if err != nil {
		return err
	}
	s, err := conn.OpenStream(ctx)
	if err != nil {
		return err
	}
	c := streamConn{s}
	defer c.Close()
	if deadline, ok := ctx.Deadline(); ok {
		s.SetDeadline(deadline)
	}
	theirs, err := roster.Sync(c, st)
	if err != nil {
		return err
	}
	if m.book.State().Mesh != "" {
		_, err = m.book.Merge(theirs)
	}
	return err
}

// setup starts a mesh with this machine in it if it is in none, and mints a
// ticket for one more machine.
func (m *meshd) setup(_ context.Context, args api.MeshSetupArgs) (api.MeshInvite, error) {
	st := m.book.State()
	switch {
	case st.Mesh == "":
		if args.Relay != "" && args.Relay != "off" {
			if u, err := url.Parse(args.Relay); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return api.MeshInvite{}, &checkout.Error{Code: "invalid", Message: fmt.Sprintf("relay %q is not an http(s) URL or off", args.Relay)}
			}
		}
		var err error
		if st, err = m.book.Create(args.Relay); err != nil {
			return api.MeshInvite{}, err
		}
	case args.Relay != "" && args.Relay != st.Relay:
		return api.MeshInvite{}, &checkout.Error{Code: "invalid", Message: fmt.Sprintf("mesh %s already uses relay %s", st.Mesh, orDefault(st.Relay, relay.Production))}
	}
	if err := m.start(st.Relay); err != nil {
		return api.MeshInvite{}, &checkout.Error{Code: "unavailable", Message: err.Error()}
	}
	n, err := m.current()
	if err != nil {
		return api.MeshInvite{}, err
	}
	if _, err := m.book.SetSelf(m.name, n.Ways()); err != nil {
		return api.MeshInvite{}, err
	}
	secret, expires, err := m.book.Invite(time.Now())
	if err != nil {
		return api.MeshInvite{}, err
	}
	tok := roster.Token{Mesh: st.Mesh, Node: m.key.ID(), Ways: n.Ways(), Relay: st.Relay, Secret: secret}
	return api.MeshInvite{Mesh: m.status(), Token: tok.String(), Expires: expires}, nil
}

// join redeems a token with the machine that minted it and takes the mesh's
// roster; the node then runs with the mesh's relay.
func (m *meshd) join(ctx context.Context, args api.MeshJoinArgs) (api.Mesh, error) {
	tok, err := roster.ParseToken(args.Token)
	if err != nil {
		return api.Mesh{}, &checkout.Error{Code: "invalid", Message: err.Error()}
	}
	cur := m.book.State()
	switch {
	case tok.Node == m.key.ID():
		return api.Mesh{}, &checkout.Error{Code: "invalid", Message: "this token was minted on this machine; run `ovnfs mesh join` on the other one"}
	case cur.Mesh == tok.Mesh && m.book.IsMember(tok.Node):
		return m.status(), nil
	case cur.Mesh != "" && len(cur.Live()) > 1:
		return api.Mesh{}, &checkout.Error{Code: "exists", Message: fmt.Sprintf("this machine is in mesh %s with other members; run `ovnfs mesh leave` first", cur.Mesh)}
	}
	m.mu.Lock()
	restart := m.node != nil && m.relayURL != relayFor(tok.Relay)
	m.mu.Unlock()
	if restart {
		m.stop()
	}
	if err := m.start(tok.Relay); err != nil {
		return api.Mesh{}, &checkout.Error{Code: "unavailable", Message: err.Error()}
	}
	st, err := m.redeem(ctx, tok)
	if err == nil {
		err = m.book.Adopt(st)
	}
	if err != nil {
		if m.book.State().Mesh == "" {
			m.stop()
		}
		return api.Mesh{}, &checkout.Error{Code: "unavailable", Message: err.Error()}
	}
	if n, err := m.current(); err == nil {
		m.book.SetSelf(m.name, n.Ways())
	}
	log.Printf("mesh %s: joined through %s", st.Mesh, tok.Node.Short())
	return m.status(), nil
}

// redeem dials the inviter at the token's ways (and the relay) with its key
// pinned and runs the join exchange.
func (m *meshd) redeem(ctx context.Context, tok roster.Token) (roster.State, error) {
	n, err := m.current()
	if err != nil {
		return roster.State{}, err
	}
	ways := slices.Clone(tok.Ways)
	if relayFor(tok.Relay) != "" {
		ways = append(ways, mesh.Way{Relay: true})
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := n.Dial(dctx, ways, tok.Node, roster.ALPN)
	if err != nil {
		return roster.State{}, fmt.Errorf("reaching %s: %w", tok.Node.Short(), err)
	}
	s, err := conn.OpenStream(dctx)
	if err != nil {
		return roster.State{}, err
	}
	c := streamConn{s}
	defer c.Close()
	s.SetDeadline(time.Now().Add(syncTimeout))
	return roster.Join(c, tok, m.name, n.Ways())
}

// rm removes another machine and tells every member, the removed one included
// (it then leaves on its own).
func (m *meshd) rm(ctx context.Context, args api.MeshRmArgs) (api.Mesh, error) {
	mem, err := m.book.Find(args.Member)
	if err != nil {
		return api.Mesh{}, &checkout.Error{Code: "not_found", Message: err.Error()}
	}
	st, err := m.book.Remove(mem.Node)
	if err != nil {
		return api.Mesh{}, err
	}
	sctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { m.syncAll(sctx, st) })
	wg.Go(func() { m.sync(sctx, mem.Node, st) })
	wg.Wait()
	return m.status(), nil
}

// leave takes this machine out of its mesh, telling the members it reaches.
func (m *meshd) leave(ctx context.Context, _ struct{}) (api.Mesh, error) {
	last, err := m.book.Leave()
	if err != nil {
		return api.Mesh{}, &checkout.Error{Code: "invalid", Message: err.Error()}
	}
	sctx, cancel := context.WithTimeout(ctx, syncTimeout)
	m.syncAll(sctx, last)
	cancel()
	m.stop()
	return m.status(), nil
}

// status describes the mesh for mesh verbs.
func (m *meshd) status() api.Mesh {
	st := m.book.State()
	out := api.Mesh{Mesh: st.Mesh, Node: m.key.ID().String(), Members: []api.MeshMember{}}
	m.mu.Lock()
	out.Relay = m.relayURL
	if m.relay != nil {
		out.RelayUp = m.relay.Up()
	}
	m.mu.Unlock()
	for _, mem := range st.Live() {
		ways := make([]string, len(mem.Ways))
		for i, w := range mem.Ways {
			ways[i] = w.String()
		}
		out.Members = append(out.Members, api.MeshMember{Node: mem.Node.String(), Name: mem.Name, Ways: ways, Self: mem.Node == m.key.ID()})
	}
	return out
}

// openLink opens an attach stream to a member over the pooled connection.
func (m *meshd) openLink(ctx context.Context, id mesh.NodeID) (io.ReadWriteCloser, error) {
	n, err := m.current()
	if err != nil {
		return nil, err
	}
	conn, err := n.Connect(ctx, id, linkALPN)
	if err != nil {
		return nil, err
	}
	s, err := conn.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	log.Printf("link to %s over %s", id.Short(), conn.Peer().Way)
	return streamConn{s}, nil
}

// streamConn is one QUIC stream as a link or membership connection; closing it
// ends both directions.
type streamConn struct{ *quic.Stream }

// Close stops reading and finishes writing.
func (c streamConn) Close() error {
	c.CancelRead(0)
	return c.Stream.Close()
}
