package node

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stencil-hq/ovnfs/mesh"
	"github.com/stencil-hq/ovnfs/mesh/relay"
)

// Pre-authentication limits, as stencil-net's gate and limiter set them.
const (
	maxPending     = 64               // handshakes and admissions under way at once
	preAuth        = 15 * time.Second // deadline from first packet to admission
	maxFailures    = 5                // failures a network may have per window
	globalFailures = 256              // failures everyone may have per window
	failureWindow  = 60 * time.Second // how long failures count
	maxTracked     = 4096             // networks tracked at most
)

var (
	errBlocked = errors.New("mesh: source rate-limited")
	errPending = errors.New("mesh: too many pending handshakes")
)

// source is where failures count: an IPv4 address, an IPv6 /64, a relayed
// peer's synthetic address, or a proven node id.
type source struct {
	ip   netip.Addr
	node mesh.NodeID
}

// addrSource is the network the address addr counts against.
func addrSource(addr net.Addr) source {
	ap := addrPort(addr)
	ip := ap.Addr().Unmap().WithZone("")
	if ip.Is6() && !relay.IsRelayed(addr) {
		ip = netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	return source{ip: ip}
}

// count is failures since a moment.
type count struct {
	n     int
	since time.Time
}

// gate refuses rate-limited sources before their handshake, bounds the
// handshakes and admissions under way, and counts failed authentication.
type gate struct {
	mu       sync.Mutex
	failures map[source]count
	global   count
	pending  int
}

func newGate() *gate { return &gate{failures: make(map[source]count)} }

// exhausted reports whether the global budget is spent at now.
func (g *gate) exhausted(now time.Time) bool {
	if g.global.n == 0 {
		return false
	}
	if now.Sub(g.global.since) >= failureWindow {
		g.global = count{}
		return false
	}
	return g.global.n >= globalFailures
}

// blocked reports whether connections from s are refused now.
func (g *gate) blocked(s source) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.blockedLocked(s, time.Now())
}

func (g *gate) blockedLocked(s source, now time.Time) bool {
	if g.exhausted(now) {
		return true
	}
	c, ok := g.failures[s]
	if !ok {
		return false
	}
	if now.Sub(c.since) >= failureWindow {
		delete(g.failures, s)
		return false
	}
	return c.n >= maxFailures
}

// fail counts a failure from s.
func (g *gate) fail(s source) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.global.n == 0 || now.Sub(g.global.since) >= failureWindow {
		g.global = count{since: now}
	}
	g.global.n++
	if _, ok := g.failures[s]; !ok && len(g.failures) >= maxTracked {
		for k, c := range g.failures {
			if now.Sub(c.since) >= failureWindow {
				delete(g.failures, k)
			}
		}
		if len(g.failures) >= maxTracked {
			var oldest source
			var at time.Time
			for k, c := range g.failures {
				if at.IsZero() || c.since.Before(at) {
					oldest, at = k, c.since
				}
			}
			delete(g.failures, oldest)
		}
	}
	c, ok := g.failures[s]
	if !ok || now.Sub(c.since) >= failureWindow {
		c = count{since: now}
	}
	c.n++
	g.failures[s] = c
}

// succeed forgets s's failures: it authenticated.
func (g *gate) succeed(s source) {
	g.mu.Lock()
	delete(g.failures, s)
	g.mu.Unlock()
}

// arrival is a connection between its first packet and its admission.
type arrival struct {
	g       *gate
	from    net.Addr
	start   time.Time
	reached atomic.Bool // admission started: the handshake completed
	once    sync.Once
}

// release frees the arrival's pending slot, once.
func (a *arrival) release() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		a.g.mu.Lock()
		a.g.pending--
		a.g.mu.Unlock()
	})
}

type arrivalKey struct{}

// connContext is the transports' quic.Transport.ConnContext: it refuses a
// rate-limited source or a connection beyond the pending bound before the
// handshake, and tracks the arrival until admission. A handshake that ends
// unfinished after the handshake deadline counts as a failure.
func (g *gate) connContext(ctx context.Context, info *quic.ClientInfo) (context.Context, error) {
	now := time.Now()
	g.mu.Lock()
	if g.blockedLocked(addrSource(info.RemoteAddr), now) {
		g.mu.Unlock()
		return nil, errBlocked
	}
	if g.pending >= maxPending {
		g.mu.Unlock()
		return nil, errPending
	}
	g.pending++
	g.mu.Unlock()
	a := &arrival{g: g, from: info.RemoteAddr, start: now}
	context.AfterFunc(ctx, func() {
		if !a.reached.Load() && time.Since(a.start) >= handshakeIdle {
			g.fail(addrSource(a.from))
		}
		a.release()
	})
	return context.WithValue(ctx, arrivalKey{}, a), nil
}

// arrivalOf is the arrival tracked for an accepted connection.
func arrivalOf(c *quic.Conn) *arrival {
	a, _ := c.Context().Value(arrivalKey{}).(*arrival)
	return a
}
