package node

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/mesh"
)

const (
	backlog = 128             // admitted connections a listener holds untaken
	signal  = 1 * time.Second // how long a listener may take to send READY
	ready   = 0x01            // the READY byte on the listener's first uni stream
)

// Listener serves one protocol: connections admitted for it arrive here in
// order. Closing it releases the protocol (later dialers get NotServing)
// and leaves the connections it handed out open.
type Listener struct {
	n       *Node
	alpn    string
	backlog chan *Conn
	done    chan struct{}
	once    sync.Once
}

// Listen claims alpn, which must be declared in Config.ALPNs; one live
// listener per protocol.
func (n *Node) Listen(alpn string) (*Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, ErrClosed
	}
	if !slices.Contains(n.alpns, alpn) {
		return nil, ErrNotDeclared
	}
	if n.claims[alpn] != nil {
		return nil, ErrListening
	}
	l := &Listener{n: n, alpn: alpn, backlog: make(chan *Conn, backlog), done: make(chan struct{})}
	n.claims[alpn] = l
	return l, nil
}

// Accept is the next admitted connection, once its dialer was sent READY.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	for {
		var c *Conn
		select {
		case <-l.done:
			return nil, ErrClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		case c = <-l.backlog:
		}
		select {
		case <-l.done:
			c.Close(mesh.NotServing, "not serving")
			return nil, ErrClosed
		default:
		}
		if !sendReady(c) {
			c.Close(0, "no ready stream")
			continue
		}
		return c, nil
	}
}

// Close releases the protocol and closes the connections still waiting in
// the backlog (NotServing).
func (l *Listener) Close() error {
	l.n.mu.Lock()
	defer l.n.mu.Unlock()
	l.releaseLocked(mesh.NotServing, "not serving")
	return nil
}

// releaseLocked ends the claim and closes the backlog with code and reason
// (node lock held).
func (l *Listener) releaseLocked(code uint64, reason string) {
	l.once.Do(func() {
		if l.n.claims[l.alpn] == l {
			delete(l.n.claims, l.alpn)
		}
		close(l.done)
	})
	for {
		select {
		case c := <-l.backlog:
			c.Close(code, reason)
		default:
			return
		}
	}
}

// deliver hands an admitted connection to its protocol's listener, closing
// it when nothing serves the protocol (NotServing) or the backlog is full
// (Busy).
func (n *Node) deliver(c *Conn) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.claims[c.alpn]
	if l == nil || n.closed {
		c.Close(mesh.NotServing, "not serving")
		return false
	}
	select {
	case l.backlog <- c:
		return true
	default:
		c.Close(mesh.Busy, "busy")
		return false
	}
}

// sendReady tells c's dialer its connection is taken: READY on a uni
// stream, finished, within the signal deadline.
func sendReady(c *Conn) bool {
	ctx, cancel := context.WithTimeout(context.Background(), signal)
	defer cancel()
	s, err := c.qc.OpenUniStreamSync(ctx)
	if err != nil {
		return false
	}
	s.SetWriteDeadline(time.Now().Add(signal))
	if _, err := s.Write([]byte{ready}); err != nil {
		return false
	}
	return s.Close() == nil
}
