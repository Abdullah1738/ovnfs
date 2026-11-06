package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stencil-hq/ovnfs/mesh"
)

const (
	stagger   = 250 * time.Millisecond // between the starts of a race's ways
	reach     = 5 * time.Second        // for the relay to route to a peer
	readyWait = 10 * time.Second       // for a listener's READY after the handshake
	busyBytes = 4096                   // bytes received per sweep that count as use (keep-alive acks stay below)
)

var (
	errNoRelay  = errors.New("mesh: no relay configured")
	errNoID     = errors.New("mesh: a relay way needs the node id")
	errNoDirect = errors.New("mesh: no direct socket")
	errNoV6     = errors.New("mesh: the direct socket has no IPv6")
	errNotNode  = errors.New("mesh: the peer presented no node certificate")
	errNoReady  = errors.New("mesh: no ready signal")
)

// poolKey is a pool entry's peer and protocol.
type poolKey struct {
	id   mesh.NodeID
	alpn string
}

// slot is a pooled connection or a dial its callers wait on.
type slot struct {
	conn   *Conn
	flight *flight
}

// flight is one dial shared by every caller of Connect for its key.
type flight struct {
	done    chan struct{}
	conn    *Conn
	err     error
	waiters int
	cancel  context.CancelFunc
}

// Connect is the pooled connection to id on alpn, dialing it (once for all
// concurrent callers) when there is none live. ctx bounds only the wait;
// the connection outlives it.
func (n *Node) Connect(ctx context.Context, id mesh.NodeID, alpn string) (*Conn, error) {
	k := poolKey{id, alpn}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil, ErrClosed
	}
	s := n.pool[k]
	if s != nil && s.conn != nil {
		if s.conn.alive() {
			s.conn.touch()
			n.mu.Unlock()
			return s.conn, nil
		}
		s = nil
	}
	if s == nil {
		fctx, cancel := context.WithCancel(n.ctx)
		s = &slot{flight: &flight{done: make(chan struct{}), cancel: cancel}}
		n.pool[k] = s
		go n.fly(fctx, k, s.flight)
	}
	f := s.flight
	f.waiters++
	n.mu.Unlock()

	select {
	case <-f.done:
		return f.conn, f.err
	case <-ctx.Done():
		n.mu.Lock()
		f.waiters--
		if f.waiters == 0 && n.pool[k] != nil && n.pool[k].flight == f {
			delete(n.pool, k)
			f.cancel()
		}
		n.mu.Unlock()
		return nil, ctx.Err()
	}
}

// fly runs a pooled dial and leaves its outcome in the pool and the flight.
func (n *Node) fly(ctx context.Context, k poolKey, f *flight) {
	defer f.cancel()
	c, err := n.dialNode(ctx, k.id, k.alpn)
	n.mu.Lock()
	if s := n.pool[k]; s != nil && s.flight == f {
		if err != nil {
			delete(n.pool, k)
		} else {
			n.pool[k] = &slot{conn: c}
		}
	} else if err == nil {
		n.offerLocked(k, c)
	}
	f.conn, f.err = c, err
	close(f.done)
	n.mu.Unlock()
}

// offerLocked pools c unless a live connection or a dial holds its key.
func (n *Node) offerLocked(k poolKey, c *Conn) {
	if s := n.pool[k]; s != nil && (s.flight != nil || s.conn.alive()) {
		return
	}
	if c.alive() {
		n.pool[k] = &slot{conn: c}
	}
}

// Dial races explicit ways to the node expect on alpn, as Connect does,
// and offers the connection to the pool. ctx bounds only the dial.
func (n *Node) Dial(ctx context.Context, ways []mesh.Way, expect mesh.NodeID, alpn string) (*Conn, error) {
	if len(ways) == 0 {
		return nil, ErrNoWays
	}
	_, c, err := n.race(ctx, ways, &expect, alpn)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.offerLocked(poolKey{expect, alpn}, c)
	n.mu.Unlock()
	return c, nil
}

// dialNode dials id for the pool: the way that last won, the peer book's
// ways, then the relay.
func (n *Node) dialNode(ctx context.Context, id mesh.NodeID, alpn string) (*Conn, error) {
	var ways []mesh.Way
	n.mu.Lock()
	if w, ok := n.memo[id]; ok {
		ways = append(ways, w)
	}
	n.mu.Unlock()
	if n.book != nil {
		for _, w := range n.book.Ways(id) {
			if !slices.Contains(ways, w) {
				ways = append(ways, w)
			}
		}
	}
	if relayWay := (mesh.Way{Relay: true}); n.relay != nil && !slices.Contains(ways, relayWay) {
		ways = append(ways, relayWay)
	}
	if len(ways) == 0 {
		return nil, ErrNoWays
	}
	way, c, err := n.race(ctx, ways, &id, alpn)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.memo[id] = way
	n.mu.Unlock()
	return c, nil
}

// class is a way's rank in a race: private networks, other direct
// addresses, then the relay.
type class int

const (
	classNetwork class = iota
	classDirect
	classRelay
)

// resolved is a race's later way with its address looked up.
type resolved struct {
	at   int
	addr netip.AddrPort
	err  error
}

// outcome is one handshake of a race.
type outcome struct {
	seq, at int
	qc      *quic.Conn
	err     error
}

// race dials ways to expect on alpn: way 0 at once, the others ordered by
// class once resolved, each a stagger after the one before or at once when
// a running one fails. The first handshake wins and is then waited on for
// READY; a peer's answer (refused, not serving, busy, no protocol) ends the
// race; else every failure comes back, in start order.
func (n *Node) race(ctx context.Context, ways []mesh.Way, expect *mesh.NodeID, alpn string) (mesh.Way, *Conn, error) {
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	results := make(chan outcome, len(ways))
	cancels := make([]context.CancelFunc, 0, len(ways))
	start := func(seq, at int, r *resolved) {
		dctx, cancel := context.WithCancel(ctx)
		cancels = append(cancels, cancel)
		go func() {
			qc, err := n.handshake(dctx, ways[at], r, expect, alpn)
			results <- outcome{seq, at, qc, err}
		}()
	}
	start(0, 0, nil)
	started, running := 1, 1
	// Late winners are closed once the race is decided.
	abandon := func() {
		for _, cancel := range cancels {
			cancel()
		}
		left := running
		go func() {
			for range left {
				if o := <-results; o.qc != nil {
					o.qc.CloseWithError(0, "")
				}
			}
		}()
	}

	orderedCh := make(chan []resolved, 1)
	go func() { orderedCh <- n.order(ctx, ways) }()
	var queue []resolved
	queued := false
	due := false
	timer := time.NewTimer(stagger)
	defer timer.Stop()
	var failures []outcome
	for {
		if due && queued && len(queue) > 0 {
			r := queue[0]
			queue = queue[1:]
			start(started, r.at, &r)
			started++
			running++
			due = false
			timer.Reset(stagger)
		}
		if running == 0 && queued && len(queue) == 0 {
			break
		}
		select {
		case o := <-results:
			running--
			if o.err == nil {
				abandon()
				c, err := n.ready(ctx, o.qc, ways[o.at], alpn)
				return ways[o.at], c, err
			}
			if ans := answer(o.err, alpn); ans != nil {
				abandon()
				return mesh.Way{}, nil, ans
			}
			failures = append(failures, o)
			due = true
		case q := <-orderedCh:
			queue, queued, orderedCh = q, true, nil
		case <-timer.C:
			due = true
		case <-ctx.Done():
			abandon()
			return mesh.Way{}, nil, ctx.Err()
		}
	}
	slices.SortFunc(failures, func(a, b outcome) int { return a.seq - b.seq })
	tried := make([]WayFailure, len(failures))
	for i, f := range failures {
		tried[i] = WayFailure{Way: ways[f.at], Err: f.err}
	}
	return mesh.Way{}, nil, &UnreachableError{Tried: tried}
}

// order resolves ways[1:] concurrently and orders them by class, stable.
func (n *Node) order(ctx context.Context, ways []mesh.Way) []resolved {
	rest := make([]resolved, len(ways)-1)
	classes := make([]class, len(ways)-1)
	done := make(chan struct{}, len(rest))
	for i := range rest {
		rest[i].at = i + 1
		go func() {
			defer func() { done <- struct{}{} }()
			w := ways[i+1]
			if w.Relay {
				classes[i] = classRelay
				return
			}
			rest[i].addr, rest[i].err = n.resolve(ctx, w)
			classes[i] = classDirect
			if rest[i].err == nil && privateNetwork(rest[i].addr.Addr()) {
				classes[i] = classNetwork
			}
		}()
	}
	for range rest {
		<-done
	}
	idx := make([]int, len(rest))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return int(classes[a]) - int(classes[b]) })
	out := make([]resolved, len(rest))
	for i, j := range idx {
		out[i] = rest[j]
	}
	return out
}

// resolve looks up a direct way's address, preferring IPv4 when the direct
// socket has no IPv6.
func (n *Node) resolve(ctx context.Context, w mesh.Way) (netip.AddrPort, error) {
	if ip, err := netip.ParseAddr(w.Host); err == nil {
		return netip.AddrPortFrom(ip.Unmap(), w.Port), nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", w.Host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("mesh: cannot resolve %s: %w", w.Host, err)
	}
	for _, ip := range ips {
		if ip = ip.Unmap(); n.v6 || ip.Is4() {
			return netip.AddrPortFrom(ip, w.Port), nil
		}
	}
	return netip.AddrPort{}, fmt.Errorf("mesh: %s has no address this node can reach", w.Host)
}

// handshake dials one way as far as the peer's key: r is its address as
// the race resolved it (nil: resolve it here).
func (n *Node) handshake(ctx context.Context, w mesh.Way, r *resolved, expect *mesh.NodeID, alpn string) (*quic.Conn, error) {
	if w.Relay {
		if n.relay == nil {
			return nil, errNoRelay
		}
		if expect == nil {
			return nil, errNoID
		}
		rctx, cancel := context.WithTimeout(ctx, reach)
		err := n.relay.Reaching(rctx, *expect)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("mesh: the relay cannot reach the node: %w", err)
		}
		return n.dialAddr(ctx, n.relayed, n.relay.AddrOf(*expect), expect, alpn)
	}
	if n.direct == nil {
		return nil, errNoDirect
	}
	var addr netip.AddrPort
	var err error
	if r != nil {
		addr, err = r.addr, r.err
	} else {
		addr, err = n.resolve(ctx, w)
	}
	if err != nil {
		return nil, err
	}
	if addr.Addr().Is6() && !n.v6 {
		return nil, errNoV6
	}
	return n.dialAddr(ctx, n.direct, net.UDPAddrFromAddrPort(addr), expect, alpn)
}

// dialAddr is the QUIC handshake with addr over tr, presenting the node's
// certificate and pinning expect.
func (n *Node) dialAddr(ctx context.Context, tr *quic.Transport, addr net.Addr, expect *mesh.NodeID, alpn string) (*quic.Conn, error) {
	qc, err := tr.Dial(ctx, addr, mesh.ClientTLS(n.key, expect, alpn), n.dialConf)
	if err == nil {
		return qc, nil
	}
	if pin, ok := errors.AsType[*mesh.PinError](err); ok {
		return nil, &WrongKeyError{Expected: pin.Expected, Seen: pin.Seen}
	}
	return nil, err
}

// ready waits for the peer's listener to take qc (READY), dialed over way:
// the connection, or the peer's answer.
func (n *Node) ready(ctx context.Context, qc *quic.Conn, way mesh.Way, alpn string) (*Conn, error) {
	unreachable := func(err error) error {
		if ans := answer(err, alpn); ans != nil {
			return ans
		}
		return &UnreachableError{Tried: []WayFailure{{Way: way, Err: err}}}
	}
	id, ok := mesh.PeerID(qc.ConnectionState().TLS)
	if !ok {
		qc.CloseWithError(0, "not a node")
		return nil, unreachable(errNotNode)
	}
	if err := readied(ctx, qc); err != nil {
		return nil, unreachable(err)
	}
	c := newConn(qc, PeerInfo{Node: id, Way: way, Relayed: way.Relay, Addr: qc.RemoteAddr()}, alpn)
	if !n.track(c) {
		return nil, ErrClosed
	}
	return c, nil
}

// readied waits for READY on the first uni stream the listener opens; the
// error is why the connection closed instead.
func readied(ctx context.Context, qc *quic.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	s, err := qc.AcceptUniStream(ctx)
	if err == nil {
		s.SetReadDeadline(time.Now().Add(readyWait))
		var b [1]byte
		if _, err = io.ReadFull(s, b[:]); err == nil && b[0] == ready {
			return nil
		}
	}
	if qc.Context().Err() != nil {
		return context.Cause(qc.Context())
	}
	// Short of a deadline or a reset stream, the error is how the
	// connection ended (the run loop may not have recorded it yet).
	_, reset := errors.AsType[*quic.StreamError](err)
	if err != nil && ctx.Err() == nil && !reset {
		return err
	}
	qc.CloseWithError(0, "no ready signal")
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return errNoReady
}

// sweep closes pooled connections unused for the pool's idle time ("0
// idle"), looking every quarter of it, until the node closes.
func (n *Node) sweep() {
	every := min(max(n.poolIdle/4, sweepMin), sweepMax)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		var idle []*Conn
		n.mu.Lock()
		for k, s := range n.pool {
			c := s.conn
			if c == nil {
				continue
			}
			if !c.alive() {
				delete(n.pool, k)
				continue
			}
			received := c.qc.ConnectionStats().BytesReceived
			if c.streams.Load() > 0 || received-c.received > busyBytes {
				c.touch()
			}
			c.received = received
			if now.Sub(time.Unix(0, c.used.Load())) >= n.poolIdle {
				delete(n.pool, k)
				idle = append(idle, c)
			}
		}
		n.mu.Unlock()
		for _, c := range idle {
			c.Close(0, "idle")
		}
	}
}
