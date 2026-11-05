package node

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/mesh"
	"github.com/quic-go/quic-go"
)

// PeerInfo is who a connection is with and how it reaches them.
type PeerInfo struct {
	Node    mesh.NodeID // the peer's proven key
	Way     mesh.Way    // the way that won the dial, or the way back to an accepted peer
	Relayed bool        // the connection runs through the relay
	Addr    net.Addr    // the remote address (synthetic when relayed)
}

// Conn is a connection to a proven peer on one protocol.
type Conn struct {
	qc   *quic.Conn
	peer PeerInfo
	alpn string

	used     atomic.Int64 // unix nanoseconds it was last handed out or opened a stream
	streams  atomic.Int64 // streams whose send side is open
	received uint64       // bytes received at the last pool sweep (pool lock)
}

// Stall detection (live.rs): the dialer's keep-alive makes both ends hear
// each other at least once a second, so a longer silence means the path or the
// peer is gone.
const (
	watchEvery = 250 * time.Millisecond // how often a connection checks it still hears its peer
	stallMin   = 3 * time.Second        // silence this long (or four RTTs, if longer) is a stall
)

func newConn(qc *quic.Conn, peer PeerInfo, alpn string) *Conn {
	c := &Conn{qc: qc, peer: peer, alpn: alpn}
	c.touch()
	go c.watch()
	return c
}

// watch closes the connection (code 0 "stalled") once it has heard nothing
// from its peer for a stall, so a crashed or unreachable peer costs its users
// seconds rather than the idle timeout: a peer that restarted cannot always
// answer with a stateless reset, which quic-go never sends in reply to packets
// as small as keep-alives. The Rust original recovers in place (rebinding, or
// checking the relay); without path migration a fresh dial is the recovery.
func (c *Conn) watch() {
	tick := time.NewTicker(watchEvery)
	defer tick.Stop()
	last, heard := c.qc.ConnectionStats().PacketsReceived, time.Now()
	for {
		select {
		case <-c.qc.Context().Done():
			return
		case now := <-tick.C:
			st := c.qc.ConnectionStats()
			if st.PacketsReceived != last {
				last, heard = st.PacketsReceived, now
				continue
			}
			if now.Sub(heard) >= max(stallMin, 4*st.SmoothedRTT) {
				c.qc.CloseWithError(0, "stalled")
				return
			}
		}
	}
}

// Peer is who the connection is with.
func (c *Conn) Peer() PeerInfo { return c.peer }

// ALPN is the protocol negotiated on the connection.
func (c *Conn) ALPN() string { return c.alpn }

// OpenStream opens a bidirectional stream, waiting for stream credit.
func (c *Conn) OpenStream(ctx context.Context) (*quic.Stream, error) {
	s, err := c.qc.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	c.track(s)
	return s, nil
}

// AcceptStream waits for the peer's next bidirectional stream.
func (c *Conn) AcceptStream(ctx context.Context) (*quic.Stream, error) {
	s, err := c.qc.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	c.track(s)
	return s, nil
}

// Exporter is 32 bytes of TLS keying material under label, equal on both
// ends of the connection.
func (c *Conn) Exporter(label string) ([32]byte, error) {
	return mesh.Exporter(c.qc.ConnectionState().TLS, label)
}

// Close ends the connection with an application close code and reason.
func (c *Conn) Close(code uint64, reason string) error {
	return c.qc.CloseWithError(quic.ApplicationErrorCode(code), reason)
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.qc.Context().Done() }

// alive reports whether the connection has not ended.
func (c *Conn) alive() bool { return c.qc.Context().Err() == nil }

// touch marks the connection used now.
func (c *Conn) touch() { c.used.Store(time.Now().UnixNano()) }

// track counts s as in use until its send side closes.
func (c *Conn) track(s *quic.Stream) {
	c.touch()
	c.streams.Add(1)
	context.AfterFunc(s.Context(), func() {
		c.streams.Add(-1)
		c.touch()
	})
}
