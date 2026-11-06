// Package node is the ovnfs mesh runtime, wire-compatible with stencil-net's
// node: QUIC over a direct UDP socket and through a relay, peers proven by
// their Ed25519 keys, per-protocol listeners that signal READY, and a pool
// of dialed connections raced over every known way to a peer.
package node

import (
	"cmp"
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stencil-hq/ovnfs/mesh"
	"github.com/stencil-hq/ovnfs/mesh/relay"
)

const (
	defaultIdle   = 30 * time.Second
	keepAlive     = 1 * time.Second       // dialer keep-alive; the accepting side sends none
	handshakeIdle = preAuth / 2           // quic-go aborts an unfinished handshake at twice this
	poolIdle      = 60 * time.Second      // pooled connections unused this long are closed
	streamCredit  = 100                   // bidi and uni streams a peer may open at once
	initialPacket = 1200                  // QUIC's minimum: fits overlay tunnels (Tailscale's MTU is 1280); MTU discovery grows it
	sweepMin      = 10 * time.Millisecond // bounds on the pool sweep period
	sweepMax      = 15 * time.Second
)

// PeerBook is where a node looks up the ways to other nodes.
type PeerBook interface {
	// Ways are the ways to id, most preferred first.
	Ways(id mesh.NodeID) []mesh.Way
}

// Config is how a node runs.
type Config struct {
	Key   *mesh.NodeKey                      // the node's identity
	Port  int                                // UDP port for direct ways; 0 = mesh.DefaultPort (else an ephemeral port if busy); -1 = no direct socket
	Relay *relay.Client                      // nil = no relay way; the caller runs it
	Book  PeerBook                           // ways to peers for Connect; nil = none but the relay
	Admit func(p PeerInfo, alpn string) bool // nil = accept all; refusal closes with code 1 "denied"
	Idle  time.Duration                      // QUIC idle timeout, default 30s
	ALPNs []string                           // protocols this node serves (listeners claim them)

	anyPort  bool          // tests: bind an ephemeral port instead of the default
	poolIdle time.Duration // tests: pooled connection disuse limit (0 = 60s)
}

// Node is a running mesh node.
type Node struct {
	key   *mesh.NodeKey
	id    mesh.NodeID
	book  PeerBook
	admit func(PeerInfo, string) bool
	relay *relay.Client
	alpns []string

	udp      *net.UDPConn
	port     int
	v6       bool // the direct socket is dual-stack
	direct   *quic.Transport
	relayed  *quic.Transport
	dialConf *quic.Config
	gate     *gate
	poolIdle time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	claims map[string]*Listener
	pool   map[poolKey]*slot
	memo   map[mesh.NodeID]mesh.Way
	conns  map[*Conn]struct{}
}

// Start binds the node's transports and serves its protocols until ctx
// ends or Close is called.
func Start(ctx context.Context, cfg Config) (*Node, error) {
	if cfg.Key == nil {
		return nil, errNoKey
	}
	idle := cfg.Idle
	if idle <= 0 {
		idle = defaultIdle
	}
	n := &Node{
		key:      cfg.Key,
		id:       cfg.Key.ID(),
		book:     cfg.Book,
		admit:    cfg.Admit,
		relay:    cfg.Relay,
		alpns:    append([]string(nil), cfg.ALPNs...),
		gate:     newGate(),
		poolIdle: cmp.Or(cfg.poolIdle, poolIdle),
		claims:   make(map[string]*Listener),
		pool:     make(map[poolKey]*slot),
		memo:     make(map[mesh.NodeID]mesh.Way),
		conns:    make(map[*Conn]struct{}),
		dialConf: &quic.Config{
			InitialPacketSize:     initialPacket,
			MaxIdleTimeout:        idle,
			KeepAlivePeriod:       keepAlive,
			MaxIncomingStreams:    streamCredit,
			MaxIncomingUniStreams: streamCredit,
		},
	}
	serverConf := &quic.Config{
		InitialPacketSize:     initialPacket,
		MaxIdleTimeout:        idle,
		HandshakeIdleTimeout:  handshakeIdle,
		MaxIncomingStreams:    streamCredit,
		MaxIncomingUniStreams: streamCredit,
	}
	// Stateless resets end a peer's connection at once when this node lost
	// its state for it (closed it long ago, or restarted), rather than at its
	// idle timeout; the key outlives restarts because it derives from the
	// node key.
	resetKey := quic.StatelessResetKey(cfg.Key.Derive("ovnfs quic stateless reset"))
	n.ctx, n.cancel = context.WithCancel(context.Background())

	type served struct {
		l       *quic.Listener
		relayed bool
	}
	var listeners []served
	fail := func(err error) (*Node, error) {
		for _, s := range listeners {
			s.l.Close()
		}
		n.closeTransports()
		n.cancel()
		return nil, err
	}
	if cfg.Port >= 0 {
		port := cfg.Port
		if port == 0 && !cfg.anyPort {
			port = mesh.DefaultPort
		}
		udp, err := listenUDP(port)
		if err != nil && cfg.Port == 0 {
			udp, err = listenUDP(0)
		}
		if err != nil {
			return fail(err)
		}
		local := udp.LocalAddr().(*net.UDPAddr)
		n.udp, n.port, n.v6 = udp, local.Port, local.IP.To4() == nil
		n.direct = &quic.Transport{Conn: udp, ConnContext: n.gate.connContext, StatelessResetKey: &resetKey}
	}
	if cfg.Relay != nil {
		n.relayed = &quic.Transport{Conn: cfg.Relay.PacketConn(), ConnContext: n.gate.connContext, StatelessResetKey: &resetKey}
	}
	if len(n.alpns) > 0 {
		tlsConf := mesh.ServerTLS(n.key, n.alpns)
		for _, tr := range []*quic.Transport{n.direct, n.relayed} {
			if tr == nil {
				continue
			}
			l, err := tr.Listen(tlsConf, serverConf)
			if err != nil {
				return fail(err)
			}
			listeners = append(listeners, served{l, tr == n.relayed})
		}
	}
	for _, s := range listeners {
		n.wg.Go(func() { n.acceptLoop(s.l, s.relayed) })
	}
	n.wg.Go(n.sweep)
	context.AfterFunc(ctx, func() { n.Close() })
	return n, nil
}

var errNoKey = errors.New("mesh: Config.Key is required")

// listenUDP binds port on every address, dual-stack where the system
// allows, else IPv4 only.
func listenUDP(port int) (*net.UDPConn, error) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		c, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	}
	return c, err
}

// ID is the node's id.
func (n *Node) ID() mesh.NodeID { return n.id }

// Close closes every connection with code 3 "restarting", ends the
// listeners and stops the transports.
func (n *Node) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	for _, l := range n.claims {
		l.releaseLocked(mesh.Restarting, "restarting")
	}
	for _, s := range n.pool {
		if s.flight != nil {
			s.flight.cancel()
		}
	}
	conns := make([]*Conn, 0, len(n.conns))
	for c := range n.conns {
		conns = append(conns, c)
	}
	n.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Go(func() { c.Close(mesh.Restarting, "restarting") })
	}
	wg.Wait()
	n.cancel()
	n.closeTransports()
	n.wg.Wait()
	return nil
}

// closeTransports stops both transports and the direct socket; the relay's
// socket stays its client's.
func (n *Node) closeTransports() {
	if n.direct != nil {
		n.direct.Close()
	}
	if n.relayed != nil {
		n.relayed.Close()
	}
	if n.udp != nil {
		n.udp.Close()
	}
}

// acceptLoop admits each connection a transport's listener accepts.
func (n *Node) acceptLoop(l *quic.Listener, relayed bool) {
	defer l.Close()
	for {
		qc, err := l.Accept(n.ctx)
		if err != nil {
			return
		}
		go n.arrive(qc, relayed)
	}
}

// arrive admits a connection whose handshake completed: a node certificate,
// the limiter by node, Admit, then the listener of its protocol.
func (n *Node) arrive(qc *quic.Conn, relayed bool) {
	a := arrivalOf(qc)
	defer a.release()
	from := addrSource(qc.RemoteAddr())
	if a != nil {
		a.reached.Store(true)
		if time.Since(a.start) > preAuth {
			qc.CloseWithError(mesh.Denied, "too slow")
			n.gate.fail(from)
			return
		}
	}
	cs := qc.ConnectionState().TLS
	id, ok := mesh.PeerID(cs)
	if !ok {
		qc.CloseWithError(mesh.Denied, "denied")
		n.gate.fail(from)
		return
	}
	src := source{node: id}
	if n.gate.blocked(src) {
		qc.CloseWithError(mesh.Denied, "denied")
		return
	}
	info := PeerInfo{Node: id, Relayed: relayed, Addr: qc.RemoteAddr()}
	if relayed {
		info.Way = mesh.Way{Relay: true}
	} else {
		info.Way = mesh.WayOf(addrPort(qc.RemoteAddr()))
	}
	alpn := cs.NegotiatedProtocol
	if n.admit != nil && !n.admit(info, alpn) {
		qc.CloseWithError(mesh.Denied, "denied")
		n.gate.fail(src)
		return
	}
	c := newConn(qc, info, alpn)
	if !n.track(c) {
		return
	}
	if n.deliver(c) {
		n.gate.succeed(src)
	}
}

// track keeps c among the node's connections until it ends (closing it
// when the node is shut down).
func (n *Node) track(c *Conn) bool {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		c.Close(mesh.Restarting, "restarting")
		return false
	}
	n.conns[c] = struct{}{}
	n.mu.Unlock()
	context.AfterFunc(c.qc.Context(), func() { n.forget(c) })
	return true
}

// forget drops an ended connection from the node and its pool.
func (n *Node) forget(c *Conn) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.conns, c)
	k := poolKey{c.peer.Node, c.alpn}
	if s := n.pool[k]; s != nil && s.conn == c {
		delete(n.pool, k)
	}
}

// addrPort is addr as an address and port (zero when it is not UDP).
func addrPort(addr net.Addr) netip.AddrPort {
	if u, ok := addr.(*net.UDPAddr); ok {
		return u.AddrPort()
	}
	ap, _ := netip.ParseAddrPort(addr.String())
	return ap
}
