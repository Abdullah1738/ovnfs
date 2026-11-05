package relay

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"
)

// syntheticPort is every synthetic address's port.
const syntheticPort = 1

// localAddr is the PacketConn's own (placeholder) address.
var localAddr = &net.UDPAddr{IP: net.IPv4(240, 0, 0, 0), Port: syntheticPort}

// synthetic is the n-th synthetic address (n from 1): 240.x.y.z:1 with the
// low 24 bits of n.
func synthetic(n uint32) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{240, byte(n >> 16), byte(n >> 8), byte(n)}), syntheticPort)
}

// IsRelayed reports whether addr is a synthetic address of a relayed peer
// (240.0.0.0/8, IPv4 or IPv4-mapped).
func IsRelayed(addr net.Addr) bool {
	ap, ok := addrPort(addr)
	return ok && ap.Addr().Is4() && ap.Addr().As4()[0] == 240
}

// addrPort is addr as an unmapped AddrPort, for UDP addresses.
func addrPort(addr net.Addr) (netip.AddrPort, bool) {
	u, ok := addr.(*net.UDPAddr)
	if !ok || u == nil {
		return netip.AddrPort{}, false
	}
	ap := u.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), ap.IsValid()
}

// PacketConn is the relay's datagram socket: WriteTo a synthetic address
// sends through the relay, ReadFrom returns relayed datagrams from their
// peers' synthetic addresses. The same PacketConn for every call; once
// closed it stays closed.
func (c *Client) PacketConn() net.PacketConn { return c.pc }

// packetConn is [Client.PacketConn]'s socket.
type packetConn struct {
	c         *Client
	closed    chan struct{}
	closeOnce sync.Once
	rd, wd    deadline
}

// newPacketConn is c's PacketConn, open, without deadlines.
func newPacketConn(c *Client) *packetConn {
	return &packetConn{c: c, closed: make(chan struct{}), rd: makeDeadline(), wd: makeDeadline()}
}

// ReadFrom blocks until a relayed datagram arrives, the read deadline
// passes or the conn closes. A datagram longer than p is truncated.
func (p *packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-p.closed:
		return 0, nil, p.opError("read", net.ErrClosed)
	case <-p.rd.wait():
		return 0, nil, p.opError("read", os.ErrDeadlineExceeded)
	default:
	}
	select {
	case d := <-p.c.inbound:
		return copy(b, d.data), net.UDPAddrFromAddrPort(d.from), nil
	case <-p.rd.wait():
		return 0, nil, p.opError("read", os.ErrDeadlineExceeded)
	case <-p.closed:
		return 0, nil, p.opError("read", net.ErrClosed)
	}
}

// WriteTo queues b for the relayed peer at synthetic address addr. It never
// blocks: a datagram the link cannot take now is dropped, as UDP would.
func (p *packetConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	select {
	case <-p.closed:
		return 0, p.opError("write", net.ErrClosed)
	case <-p.wd.wait():
		return 0, p.opError("write", os.ErrDeadlineExceeded)
	default:
	}
	ap, ok := addrPort(addr)
	if !ok {
		return 0, p.opError("write", errors.New("not a relayed peer's address"))
	}
	p.c.send(ap, b)
	return len(b), nil
}

// Close ends the PacketConn: blocked and later reads and writes fail. The
// relay link itself runs until Run's context ends.
func (p *packetConn) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

// LocalAddr is a synthetic placeholder (240.0.0.0:1); the relay link has no
// address of its own.
func (p *packetConn) LocalAddr() net.Addr { return localAddr }

// SetReadBuffer is accepted and ignored: the relay link's own queues are the
// socket's buffers. Having it tells quic-go the socket is not a broken UDP one.
func (p *packetConn) SetReadBuffer(int) error { return nil }

// SetWriteBuffer is accepted and ignored, like SetReadBuffer.
func (p *packetConn) SetWriteBuffer(int) error { return nil }

// SetDeadline sets both the read and the write deadline.
func (p *packetConn) SetDeadline(t time.Time) error {
	p.rd.set(t)
	p.wd.set(t)
	return nil
}

// SetReadDeadline bounds blocked and future ReadFrom calls (zero: none).
func (p *packetConn) SetReadDeadline(t time.Time) error {
	p.rd.set(t)
	return nil
}

// SetWriteDeadline makes WriteTo fail once t passed (zero: never).
func (p *packetConn) SetWriteDeadline(t time.Time) error {
	p.wd.set(t)
	return nil
}

// opError wraps err as the net package reports socket errors.
func (p *packetConn) opError(op string, err error) error {
	return &net.OpError{Op: op, Net: "relay", Addr: localAddr, Err: err}
}

// deadline is a settable deadline whose channel closes when it passes
// (net.Pipe's pattern).
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{} // closed when the deadline passed
}

// makeDeadline is a deadline that never passes until set.
func makeDeadline() deadline { return deadline{cancel: make(chan struct{})} }

// set moves the deadline to t (zero: none).
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // the timer fired: wait for it to close cancel
	}
	d.timer = nil
	closed := isClosed(d.cancel)
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		cancel := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(cancel) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

// wait is closed once the deadline passed.
func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

// isClosed reports whether c is closed.
func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
