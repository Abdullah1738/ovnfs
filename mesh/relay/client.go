// Package relay is a node's link to a stencil relay (`GET /node`, protocols
// stencil-relay.v2 and v1): it proves the node's identity, keeps the link
// up, and carries datagrams to and from peers reachable only through the
// relay. Those peers appear behind synthetic IPv4 addresses (240.x.y.z:1)
// on the [net.PacketConn] that [Client.PacketConn] returns, so a QUIC
// transport can run over the relay as if it were a UDP socket.
//
// The behaviour follows stencil-net's client (crates/net/src/relay/link.rs):
// routes are per link and forgotten when it ends; peers used in the last
// minute are asked for again when it comes back; datagrams to a peer
// without a route wait (a few) on v1 links and go ahead as send-to frames
// on v2 links.
package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/stencil-hq/ovnfs/mesh"
)

// Production is the stencil relay nodes use by default.
const Production = "https://my.omp.sh"

const (
	dialTimeout  = 10 * time.Second       // TCP to the answer to the relay's challenge
	writeTimeout = 10 * time.Second       // one batch of frames to the relay
	pingEvery    = 15 * time.Second       // an idle link pings this often
	pongWait     = 5 * time.Second        // a ping unanswered this long ends the link
	shortLived   = 5 * time.Second        // a link shorter than this counts as a failed dial
	outCap       = 512 << 10              // queued frame bytes before datagrams are dropped
	sojourn      = 150 * time.Millisecond // a datagram queued longer is dropped
	inboundCap   = 4096                   // relayed datagrams awaiting ReadFrom
	waitingCap   = 4                      // datagrams held for a peer without a route (v1)
	askAgain     = 250 * time.Millisecond // a peer without a route is asked for again after
	recent       = 60 * time.Second       // peers used this recently are re-asked on link up
	batchMax     = 64 << 10               // the largest message the writer sends
)

// ErrNoPeer reports that the relay has no link to the peer asked for.
var ErrNoPeer = errors.New("relay: peer is not connected to the relay")

// errEnded: the link ended without the relay saying why.
var errEnded = errors.New("relay: link ended")

// closedError is the relay's closing frame: why it ended the link and how
// long it asked the node to stay away.
type closedError struct {
	reason closeReason
	retry  time.Duration
}

func (e *closedError) Error() string {
	return fmt.Sprintf("relay closed the link: %s (retry in %s)", e.reason, e.retry)
}

// Client is one node's link to one relay, kept up by [Client.Run].
type Client struct {
	url    string // ws:// or wss:// URL of /node; empty when urlErr is set
	urlErr error
	key    *mesh.NodeKey
	id     mesh.NodeID
	http   *http.Client
	offer  []string // subprotocols offered, most preferred first

	inbound  chan datagram // relayed datagrams for the PacketConn
	pc       *packetConn
	welcomed atomic.Bool // the relay welcomed the current (or last) link

	mu       sync.Mutex
	peers    map[mesh.NodeID]*slot
	addrs    map[netip.AddrPort]mesh.NodeID // synthetic address → peer
	routes   map[uint32]mesh.NodeID         // current link's route → peer
	count    uint32                         // synthetic addresses handed out
	out      outQueue
	up       bool
	links    uint64 // links that came up so far
	lastErr  error  // why the last dial or link failed
	observed netip.AddrPort
	changed  chan struct{} // closed and replaced on every state change
	wake     chan struct{} // wakes the writer
}

// slot is one relayed peer.
type slot struct {
	addr    netip.AddrPort // its synthetic address
	route   uint32         // its route on the current link, when routed
	routed  bool
	asked   time.Time // when it was last asked for on this link (zero: not)
	used    time.Time // when a datagram last went to it
	open    bool      // a Connect for it went out on this link and no NoPeer answered
	waiting [][]byte  // datagrams held until its route arrives
	noPeers uint64    // NoPeer answers for it so far
}

// datagram is a relayed datagram on its way to ReadFrom.
type datagram struct {
	from netip.AddrPort
	data []byte
}

// New makes the link of the node key proves to the relay at url (http:// or
// https://, any path; the link always goes to /node). Nothing happens until
// [Client.Run].
func New(rawURL string, key *mesh.NodeKey) *Client {
	c := &Client{
		key:     key,
		id:      key.ID(),
		http:    newHTTPClient(),
		offer:   []string{protocolV2, protocolV1},
		inbound: make(chan datagram, inboundCap),
		peers:   make(map[mesh.NodeID]*slot),
		addrs:   make(map[netip.AddrPort]mesh.NodeID),
		routes:  make(map[uint32]mesh.NodeID),
		changed: make(chan struct{}),
		wake:    make(chan struct{}, 1),
	}
	c.url, c.urlErr = linkURL(rawURL)
	c.pc = newPacketConn(c)
	return c
}

// linkURL is the WebSocket URL of the relay at raw's /node.
func linkURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("relay url %q: %w", raw, err)
	}
	scheme := ""
	switch u.Scheme {
	case "https", "wss":
		scheme = "wss"
	case "http", "ws":
		scheme = "ws"
	default:
		return "", fmt.Errorf("relay url %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("relay url %q: no host", raw)
	}
	return (&url.URL{Scheme: scheme, Host: u.Host, Path: nodePath}).String(), nil
}

// newHTTPClient is the client links are dialed with: HTTP/1.1 only (the
// relay upgrades only HTTP/1.1; HTTP/2 gets 426), proxies from the
// environment, and TCP_NOTSENT_LOWAT where the OS has it.
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: notsentLowat}
	tr.DialContext = dialer.DialContext
	tr.ForceAttemptHTTP2 = false
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	tr.Protocols = &protocols
	return &http.Client{Transport: tr}
}

// Up reports whether the link is up now.
func (c *Client) Up() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.up
}

// Observed is the node's own address as the relay saw it on the last link
// that was welcomed (its port is 0 behind a relay's TLS terminator).
func (c *Client) Observed() (netip.AddrPort, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observed, c.observed.IsValid()
}

// AddrOf is peer's synthetic address, stable for the Client's life. A route
// is asked for on the first datagram sent to it.
func (c *Client) AddrOf(peer mesh.NodeID) *net.UDPAddr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return net.UDPAddrFromAddrPort(c.slot(peer, time.Now()).addr)
}

// PeerOf is the relayed peer behind synthetic address addr.
func (c *Client) PeerOf(addr net.Addr) (mesh.NodeID, bool) {
	ap, ok := addrPort(addr)
	if !ok {
		return mesh.NodeID{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	peer, ok := c.addrs[ap]
	return peer, ok
}

// Reaching asks the relay for a route to peer, waiting for the link to be
// up first: nil once the relay routed the peer, [ErrNoPeer] when the peer
// has no link to the relay, the link's error when it is down or ends first,
// ctx's error when ctx ends first.
func (c *Client) Reaching(ctx context.Context, peer mesh.NodeID) error {
	c.mu.Lock()
	for !c.up {
		changed, last := c.changed, c.lastErr
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("relay link down: %w", last)
			}
			return ctx.Err()
		case <-changed:
		}
		c.mu.Lock()
	}
	now := time.Now()
	link := c.links
	s := c.slot(peer, now)
	if s.routed {
		c.mu.Unlock()
		return nil
	}
	noPeers := s.noPeers
	c.ask(peer, s, now)
	c.mu.Unlock()
	c.wakeWriter()
	for {
		c.mu.Lock()
		switch {
		case c.links != link || !c.up:
			err := c.lastErr
			c.mu.Unlock()
			if err == nil {
				err = errEnded
			}
			return err
		case s.routed:
			c.mu.Unlock()
			return nil
		case s.noPeers != noPeers:
			c.mu.Unlock()
			return ErrNoPeer
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// Run keeps the link up until ctx ends: dials (within 10 s), carries the
// link while it lasts, waits as the relay asked or backs off, and dials
// again. Call it once.
func (c *Client) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		var wait time.Duration
		conn, v2, err := c.dial(ctx)
		if err == nil {
			began := time.Now()
			ended := c.carry(ctx, conn, v2)
			welcomed := c.welcomed.Load()
			if welcomed && time.Since(began) >= shortLived {
				failures = 0
			} else {
				failures++
			}
			wait = redialAfter(ended, failures, welcomed)
			c.wentDown(ended)
		} else {
			failures++
			wait = redialAfter(err, failures, false)
			c.mu.Lock()
			c.lastErr = err
			c.mu.Unlock()
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
}

// redialAfter is how long to wait before dialing again after err, the
// failures-th in a row, on a link the relay welcomed (lived) or not: what
// the relay asked for when it closed a link that lived, else backoff, or
// longer when the relay asked for longer.
func redialAfter(err error, failures int, lived bool) time.Duration {
	wait := backoff(failures + 1)
	if closed, ok := errors.AsType[*closedError](err); ok {
		if lived {
			return closed.retry
		}
		return max(closed.retry, wait)
	}
	return wait
}

// backoff is the wait before reconnect attempt n (from 1): none for the
// first, then 250 ms doubling to 2 s, less up to a fifth at random so nodes
// that lost the same relay do not stampede.
func backoff(n int) time.Duration {
	if n <= 1 {
		return 0
	}
	base := min(250*time.Millisecond<<min(n-2, 3), 2*time.Second)
	return base - time.Duration(rand.Int64N(int64(base/5)+1))
}

// dial opens a link and answers the relay's challenge; whether the relay
// chose v2.
func (c *Client) dial(ctx context.Context) (*websocket.Conn, bool, error) {
	if c.urlErr != nil {
		return nil, false, c.urlErr
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, c.url, &websocket.DialOptions{
		HTTPClient:      c.http,
		Subprotocols:    c.offer,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, false, err
	}
	conn.SetReadLimit(maxMessage)
	v2 := conn.Subprotocol() == protocolV2
	for {
		typ, msg, err := conn.Read(ctx)
		if err != nil {
			conn.CloseNow()
			return nil, false, err
		}
		if typ != websocket.MessageBinary {
			continue
		}
		r := frameReader{rest: msg}
		f, ok := r.next()
		if !ok {
			if r.err != nil {
				conn.CloseNow()
				return nil, false, r.err
			}
			continue
		}
		switch f.kind {
		case kindChallenge:
			auth := frame{kind: kindAuth, peer: c.id, sig: [64]byte(c.key.Sign(challengeMessage(f.nonce)))}
			if err := conn.Write(ctx, websocket.MessageBinary, appendFrame(nil, &auth)); err != nil {
				conn.CloseNow()
				return nil, false, err
			}
			return conn, v2, nil
		case kindClosing:
			conn.CloseNow()
			return nil, false, &closedError{reason: f.reason, retry: f.retry}
		}
	}
}

// carry runs the link until it ends (or ctx does) and says why: a reader
// goroutine handles the relay's messages, this one writes queued frames and
// pings.
func (c *Client) carry(ctx context.Context, conn *websocket.Conn, v2 bool) error {
	// The reader outlives ctx so a closing handshake can finish.
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRead()
	c.cameUp(v2)
	readErr := make(chan error, 1)
	pongs := make(chan uint64, 1)
	go func() { readErr <- c.read(readCtx, conn, pongs) }()

	var (
		pinged   uint64
		pongDue  time.Time
		nextPing = time.Now().Add(pingEvery)
		spare    []byte
		ends     []int
	)
	timer := time.NewTimer(pingEvery)
	defer timer.Stop()
	flush := func() error {
		c.mu.Lock()
		buf, frameEnds := c.out.take(time.Now(), spare, ends[:0])
		c.mu.Unlock()
		spare, ends = buf, frameEnds
		if len(buf) == 0 {
			return nil
		}
		wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
		defer wcancel()
		start, cut := 0, 0
		for _, end := range frameEnds {
			if end-start > batchMax && cut > start {
				if err := conn.Write(wctx, websocket.MessageBinary, buf[start:cut]); err != nil {
					return err
				}
				start = cut
			}
			cut = end
		}
		if cut > start {
			return conn.Write(wctx, websocket.MessageBinary, buf[start:cut])
		}
		return nil
	}
	ping := func(now time.Time, wait time.Duration) {
		pinged++
		pongDue = now.Add(wait)
		c.mu.Lock()
		c.out.push(&frame{kind: kindPing, n: pinged}, now)
		c.mu.Unlock()
		c.wakeWriter()
	}
	for {
		wakeAt := nextPing
		if !pongDue.IsZero() && pongDue.Before(wakeAt) {
			wakeAt = pongDue
		}
		timer.Reset(time.Until(wakeAt))
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return ctx.Err()
		case err := <-readErr:
			conn.CloseNow()
			return err
		case n := <-pongs:
			if !pongDue.IsZero() && n == pinged {
				pongDue = time.Time{}
			}
		case <-c.wake:
			if err := flush(); err != nil {
				conn.CloseNow()
				return err
			}
		case now := <-timer.C:
			if !pongDue.IsZero() && !now.Before(pongDue) {
				conn.CloseNow()
				return fmt.Errorf("relay: no pong within %s", pongWait)
			}
			if !now.Before(nextPing) {
				nextPing = now.Add(pingEvery)
				if pongDue.IsZero() {
					ping(now, pongWait)
				}
			}
		}
	}
}

// read handles the relay's messages until the link fails or the relay
// closes it, passing pongs to the writer.
func (c *Client) read(ctx context.Context, conn *websocket.Conn, pongs chan uint64) error {
	for {
		typ, msg, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				return errEnded
			}
			return err
		}
		if typ != websocket.MessageBinary {
			continue
		}
		pong, hasPong, err := c.onMessage(msg)
		if err != nil {
			return err
		}
		if hasPong {
			select {
			case <-pongs:
			default:
			}
			pongs <- pong
		}
	}
}

// onMessage handles one message from the relay; the pong it carried, if any.
func (c *Client) onMessage(msg []byte) (pong uint64, hasPong bool, err error) {
	r := frameReader{rest: msg}
	for {
		f, ok := r.next()
		if !ok {
			return pong, hasPong, r.err
		}
		switch f.kind {
		case kindRecv:
			c.mu.Lock()
			peer, ok := c.routes[f.route]
			var from netip.AddrPort
			if ok {
				from = c.peers[peer].addr
			}
			c.mu.Unlock()
			if ok {
				select {
				case c.inbound <- datagram{from: from, data: f.datagram}:
				default:
				}
			}
		case kindRoute:
			c.routedTo(f.peer, f.route)
		case kindNoPeer:
			c.noPeer(f.peer)
		case kindGone:
			c.gone(f.route)
		case kindPong:
			pong, hasPong = f.n, true
		case kindWelcome:
			c.mu.Lock()
			c.observed = f.observed
			c.mu.Unlock()
			c.welcomed.Store(true)
		case kindClosing:
			return pong, hasPong, &closedError{reason: f.reason, retry: f.retry}
		}
	}
}

// wakeWriter wakes the link's writer.
func (c *Client) wakeWriter() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// changedLocked tells waiters the link or a route changed. c.mu held.
func (c *Client) changedLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// cameUp starts a link speaking v2 when v2: frames may be queued, old routes
// are forgotten, recent peers are asked for again (on v2, what waited for
// them goes along).
func (c *Client) cameUp(v2 bool) {
	now := time.Now()
	c.welcomed.Store(false)
	c.mu.Lock()
	c.forgetRoutes()
	c.out.clear()
	c.out.live, c.out.v2 = true, v2
	for peer, s := range c.peers {
		if len(s.waiting) == 0 && now.Sub(s.used) >= recent {
			continue
		}
		if c.ask(peer, s, now) && v2 {
			waiting := s.waiting
			s.waiting = nil
			for _, d := range waiting {
				c.carryTo(peer, s, d, now)
			}
		}
	}
	c.up = true
	c.links++
	c.lastErr = nil
	c.changedLocked()
	c.mu.Unlock()
	c.wakeWriter()
}

// wentDown ends the link for why: nothing more is queued, and its routes
// are gone.
func (c *Client) wentDown(why error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out.live = false
	c.out.clear()
	c.forgetRoutes()
	c.up = false
	c.lastErr = why
	c.changedLocked()
}

// forgetRoutes forgets every route and what was asked for. c.mu held.
func (c *Client) forgetRoutes() {
	clear(c.routes)
	for _, s := range c.peers {
		s.route, s.routed, s.asked, s.open = 0, false, time.Time{}, false
	}
}

// routedTo records the relay's route to peer, sends what waited for it,
// and tells whoever asked.
func (c *Client) routedTo(peer mesh.NodeID, route uint32) {
	now := time.Now()
	c.mu.Lock()
	s := c.slot(peer, now)
	if s.routed {
		delete(c.routes, s.route)
	}
	s.route, s.routed = route, true
	c.routes[route] = peer
	queued := false
	waiting := s.waiting
	s.waiting = nil
	for _, d := range waiting {
		queued = c.carryTo(peer, s, d, now) || queued
	}
	c.changedLocked()
	c.mu.Unlock()
	if queued {
		c.wakeWriter()
	}
}

// noPeer drops what waited for peer, which has no link to the relay;
// nothing more goes to it until it is asked for again.
func (c *Client) noPeer(peer mesh.NodeID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.peers[peer]; ok {
		s.waiting = nil
		s.open = false
		s.noPeers++
		c.changedLocked()
	}
}

// gone ends route: its peer has none until asked for again (on its next
// datagram).
func (c *Client) gone(route uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	peer, ok := c.routes[route]
	if !ok {
		return
	}
	delete(c.routes, route)
	if s := c.peers[peer]; s.routed && s.route == route {
		s.route, s.routed, s.asked, s.open = 0, false, time.Time{}, false
		c.changedLocked()
	}
}

// slot is peer's slot, made with the next synthetic address when new.
// c.mu held.
func (c *Client) slot(peer mesh.NodeID, now time.Time) *slot {
	s, ok := c.peers[peer]
	if !ok {
		c.count++
		s = &slot{addr: synthetic(c.count), used: now}
		c.peers[peer] = s
		c.addrs[s.addr] = peer
	}
	return s
}

// ask queues a Connect for peer; whether it was queued (not while the link
// is down or its queue full). c.mu held.
func (c *Client) ask(peer mesh.NodeID, s *slot, now time.Time) bool {
	s.asked = now
	if !c.out.push(&frame{kind: kindConnect, peer: peer}, now) {
		return false
	}
	s.open = true
	return true
}

// carryTo queues d for peer: on its route; else, asked for on a v2 link, as
// send-to; else held (a few) for the route. Whether a frame was queued.
// c.mu held.
func (c *Client) carryTo(peer mesh.NodeID, s *slot, d []byte, now time.Time) bool {
	switch {
	case s.routed:
		return len(d) <= maxDatagram && c.out.push(&frame{kind: kindSend, route: s.route, datagram: d}, now)
	case c.out.v2 && s.open && len(d) <= maxSendTo:
		return c.out.push(&frame{kind: kindSendTo, peer: peer, datagram: d}, now)
	default:
		if len(s.waiting) < waitingCap && len(d) <= maxDatagram {
			s.waiting = append(s.waiting, bytes.Clone(d))
		}
		return false
	}
}

// send queues datagram d for the relayed peer at synthetic address to,
// asking for a route when it has none. Dropped, as UDP would drop it, when
// the address is unknown, the link is down or its queue is full.
func (c *Client) send(to netip.AddrPort, d []byte) {
	now := time.Now()
	c.mu.Lock()
	peer, ok := c.addrs[to]
	if !ok {
		c.mu.Unlock()
		return
	}
	s := c.peers[peer]
	s.used = now
	queued := false
	if !s.routed && (s.asked.IsZero() || now.Sub(s.asked) >= askAgain) {
		queued = c.ask(peer, s, now)
	}
	queued = c.carryTo(peer, s, d, now) || queued
	c.mu.Unlock()
	if queued {
		c.wakeWriter()
	}
}
