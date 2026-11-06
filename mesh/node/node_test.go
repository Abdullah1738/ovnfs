package node

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stencil-hq/ovnfs/mesh"
	"github.com/stencil-hq/ovnfs/mesh/relay"
)

// book is a fixed peer book.
type book struct {
	mu   sync.Mutex
	ways map[mesh.NodeID][]mesh.Way
}

func (b *book) Ways(id mesh.NodeID) []mesh.Way {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ways[id]
}

func (b *book) set(id mesh.NodeID, ways ...mesh.Way) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ways == nil {
		b.ways = make(map[mesh.NodeID][]mesh.Way)
	}
	b.ways[id] = ways
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// start runs a node on an ephemeral loopback-reachable port until the test ends.
func start(t *testing.T, cfg Config) *Node {
	t.Helper()
	if cfg.Key == nil {
		key, err := mesh.LoadOrCreateKey(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Key = key
	}
	cfg.anyPort = true
	n, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

// loopWay is n's direct way on IPv4 loopback.
func loopWay(n *Node) mesh.Way {
	return mesh.Way{Host: "127.0.0.1", Port: uint16(n.port)}
}

// serve accepts on l until it closes, echoing every stream back.
func serve(t *testing.T, l *Listener) <-chan *Conn {
	accepted := make(chan *Conn, 16)
	go func() {
		for {
			c, err := l.Accept(context.Background())
			if err != nil {
				return
			}
			accepted <- c
			go func() {
				for {
					s, err := c.AcceptStream(context.Background())
					if err != nil {
						return
					}
					go func() {
						io.Copy(s, s)
						s.Close()
					}()
				}
			}()
		}
	}()
	return accepted
}

// roundTrip writes msg on a new stream of c and reads the echo.
func roundTrip(t *testing.T, c *Conn, msg string) {
	t.Helper()
	ctx := testCtx(t)
	s, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := s.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	got, err := io.ReadAll(s)
	if err != nil || string(got) != msg {
		t.Fatalf("echo %q, %v", got, err)
	}
}

func TestConnectByIDOverPeerBook(t *testing.T) {
	a := start(t, Config{ALPNs: []string{"test"}})
	l, err := a.Listen("test")
	if err != nil {
		t.Fatal(err)
	}
	accepted := serve(t, l)
	var bk book
	bk.set(a.ID(), loopWay(a))
	b := start(t, Config{Book: &bk})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, err := b.Connect(ctx, a.ID(), "test")
	cancel() // the connection outlives the dial's context
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Peer(); p.Node != a.ID() || p.Way != loopWay(a) || p.Relayed || c.ALPN() != "test" {
		t.Fatalf("dialed peer %+v alpn %q", p, c.ALPN())
	}
	roundTrip(t, c, "hello over the mesh")
	ac := <-accepted
	if p := ac.Peer(); p.Node != b.ID() || p.Relayed || p.Way.Relay {
		t.Fatalf("accepted peer %+v", p)
	}
	e1, err1 := c.Exporter("EXPORTER-test")
	e2, err2 := ac.Exporter("EXPORTER-test")
	if err1 != nil || err2 != nil || e1 != e2 {
		t.Fatalf("exporters differ: %x %x", e1, e2)
	}

	// Pooled: the same connection again.
	again, err := b.Connect(testCtx(t), a.ID(), "test")
	if err != nil || again != c {
		t.Fatalf("second Connect: %p vs %p, %v", again, c, err)
	}

	// Once the peer closes it, Connect dials anew.
	ac.Close(0, "bye")
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("dialed side never saw the close")
	}
	fresh, err := b.Connect(testCtx(t), a.ID(), "test")
	if err != nil || fresh == c {
		t.Fatalf("Connect after close: %v (same conn: %v)", err, fresh == c)
	}
	roundTrip(t, fresh, "again")
}

// TestRestartedPeerEndsStaleConnections checks a node that crashed (its socket
// gone, no close sent) and came back with the same key and port ends the
// connections peers still hold at once, by stateless reset, instead of leaving
// them to their idle timeout.
func TestRestartedPeerEndsStaleConnections(t *testing.T) {
	key, err := mesh.LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := start(t, Config{Key: key, ALPNs: []string{"test"}})
	l, err := a.Listen("test")
	if err != nil {
		t.Fatal(err)
	}
	serve(t, l)
	var bk book
	bk.set(a.ID(), loopWay(a))
	b := start(t, Config{Book: &bk})
	c, err := b.Connect(testCtx(t), a.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c, "before the crash")

	port := a.port
	a.udp.Close()
	restarted, err := Start(context.Background(), Config{Key: key, Port: port, ALPNs: []string{"test"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection outlived its peer's restart until the idle timeout")
	}
}

// TestVanishedPeerStalls checks a connection whose peer went silent (crashed,
// nothing listening any more) ends after the stall period, not the idle timeout.
func TestVanishedPeerStalls(t *testing.T) {
	a := start(t, Config{ALPNs: []string{"test"}})
	l, err := a.Listen("test")
	if err != nil {
		t.Fatal(err)
	}
	serve(t, l)
	var bk book
	bk.set(a.ID(), loopWay(a))
	b := start(t, Config{Book: &bk})
	c, err := b.Connect(testCtx(t), a.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c, "before it vanishes")
	a.udp.Close()
	select {
	case <-c.Done():
	case <-time.After(stallMin + 3*time.Second):
		t.Fatal("the connection to a vanished peer outlived the stall period")
	}
}

func TestConcurrentConnectsShareOneDial(t *testing.T) {
	a := start(t, Config{ALPNs: []string{"test"}})
	l, _ := a.Listen("test")
	accepted := serve(t, l)
	var bk book
	bk.set(a.ID(), loopWay(a))
	b := start(t, Config{Book: &bk})
	var wg sync.WaitGroup
	conns := make([]*Conn, 8)
	for i := range conns {
		wg.Go(func() {
			c, err := b.Connect(testCtx(t), a.ID(), "test")
			if err != nil {
				t.Error(err)
			}
			conns[i] = c
		})
	}
	wg.Wait()
	for _, c := range conns[1:] {
		if c != conns[0] {
			t.Fatal("concurrent Connects got different connections")
		}
	}
	<-accepted
	select {
	case <-accepted:
		t.Fatal("more than one connection accepted")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWrongKeyIsRefused(t *testing.T) {
	a := start(t, Config{ALPNs: []string{"test"}})
	l, _ := a.Listen("test")
	serve(t, l)
	b := start(t, Config{})
	other, err := mesh.LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Dial(testCtx(t), []mesh.Way{loopWay(a)}, other.ID(), "test")
	wrong, ok := errors.AsType[*WrongKeyError](err)
	if !ok || wrong.Seen != a.ID() || wrong.Expected != other.ID() {
		t.Fatalf("want WrongKeyError, got %v", err)
	}
}

func TestAdmissionRefusal(t *testing.T) {
	var asked PeerInfo
	var mu sync.Mutex
	a := start(t, Config{ALPNs: []string{"test"}, Admit: func(p PeerInfo, alpn string) bool {
		mu.Lock()
		asked = p
		mu.Unlock()
		return alpn != "test"
	}})
	l, _ := a.Listen("test")
	serve(t, l)
	b := start(t, Config{})
	_, err := b.Dial(testCtx(t), []mesh.Way{loopWay(a)}, a.ID(), "test")
	refused, ok := errors.AsType[*RefusedError](err)
	if !ok || refused.Reason != "denied" {
		t.Fatalf("want RefusedError denied, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked.Node != b.ID() {
		t.Fatalf("Admit saw %+v", asked)
	}
}

func TestProtocolAnswers(t *testing.T) {
	a := start(t, Config{ALPNs: []string{"test", "idle"}})
	l, _ := a.Listen("test")
	serve(t, l)
	b := start(t, Config{})
	ways := []mesh.Way{loopWay(a)}
	_, err := b.Dial(testCtx(t), ways, a.ID(), "unknown")
	if e, ok := errors.AsType[*NoProtocolError](err); !ok || e.ALPN != "unknown" {
		t.Fatalf("undeclared protocol: %v", err)
	}
	_, err = b.Dial(testCtx(t), ways, a.ID(), "idle")
	if e, ok := errors.AsType[*NotServingError](err); !ok || e.ALPN != "idle" {
		t.Fatalf("declared but unserved protocol: %v", err)
	}
	if _, err := a.Listen("test"); !errors.Is(err, ErrListening) {
		t.Fatalf("second listener: %v", err)
	}
	if _, err := a.Listen("other"); !errors.Is(err, ErrNotDeclared) {
		t.Fatalf("undeclared listener: %v", err)
	}
	// A closed listener frees the protocol: dialers get NotServing.
	l.Close()
	_, err = b.Dial(testCtx(t), ways, a.ID(), "test")
	if _, ok := errors.AsType[*NotServingError](err); !ok {
		t.Fatalf("after listener close: %v", err)
	}
}

func TestNoWaysAndUnreachable(t *testing.T) {
	b := start(t, Config{Book: &book{}})
	a := start(t, Config{ALPNs: []string{"test"}})
	if _, err := b.Connect(testCtx(t), a.ID(), "test"); !errors.Is(err, ErrNoWays) {
		t.Fatalf("no ways: %v", err)
	}
	// A closed port and a bad name: both fail, listed in start order.
	dead, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	deadPort := dead.LocalAddr().(*net.UDPAddr).Port
	dead.Close()
	ways := []mesh.Way{
		{Host: "127.0.0.1", Port: uint16(deadPort)},
		{Host: "no-such-host.invalid", Port: 1},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := b.Dial(ctx, ways, a.ID(), "test")
	un, ok := errors.AsType[*UnreachableError](err)
	if !ok || len(un.Tried) != 2 || un.Tried[0].Way != ways[0] || un.Tried[1].Way != ways[1] {
		t.Fatalf("want Unreachable over both ways, got %v", err)
	}
}

func TestRaceSkipsDeadWayAndMemoizesWinner(t *testing.T) {
	a := start(t, Config{ALPNs: []string{"test"}})
	l, _ := a.Listen("test")
	serve(t, l)
	dead := mesh.Way{Host: "no-such-host.invalid", Port: 1}
	var bk book
	bk.set(a.ID(), dead, loopWay(a))
	b := start(t, Config{Book: &bk})
	c, err := b.Connect(testCtx(t), a.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if c.Peer().Way != loopWay(a) {
		t.Fatalf("won over %v", c.Peer().Way)
	}
	b.mu.Lock()
	memo := b.memo[a.ID()]
	b.mu.Unlock()
	if memo != loopWay(a) {
		t.Fatalf("memo %v", memo)
	}
}

func TestCloseRestartsPeersAndEndsAccept(t *testing.T) {
	a := start(t, Config{ALPNs: []string{"test"}})
	l, _ := a.Listen("test")
	var bk book
	bk.set(a.ID(), loopWay(a))
	b := start(t, Config{Book: &bk})
	got := make(chan *Conn, 1)
	go func() {
		c, _ := l.Accept(context.Background())
		got <- c
	}()
	c, err := b.Connect(testCtx(t), a.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	ac := <-got
	streamErr := make(chan error, 1)
	go func() {
		_, err := ac.AcceptStream(context.Background())
		streamErr <- err
	}()
	acceptErr := make(chan error, 1)
	go func() {
		_, err := l.Accept(context.Background())
		acceptErr <- err
	}()
	a.Close()
	for name, ch := range map[string]chan error{"Accept": acceptErr, "AcceptStream": streamErr} {
		select {
		case err := <-ch:
			if err == nil {
				t.Fatalf("%s returned no error after Close", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s still blocked after Close", name)
		}
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("peer never saw the close")
	}
	ae, ok := errors.AsType[*quic.ApplicationError](context.Cause(c.qc.Context()))
	if !ok || ae.ErrorCode != mesh.Restarting || ae.ErrorMessage != "restarting" {
		t.Fatalf("close reason %v", context.Cause(c.qc.Context()))
	}
	if _, err := a.Connect(testCtx(t), b.ID(), "test"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Connect on a closed node: %v", err)
	}
}

func TestIdlePooledConnectionIsClosed(t *testing.T) {
	a := start(t, Config{ALPNs: []string{"test"}})
	l, _ := a.Listen("test")
	serve(t, l)
	var bk book
	bk.set(a.ID(), loopWay(a))
	b := start(t, Config{Book: &bk, poolIdle: 300 * time.Millisecond})
	c, err := b.Connect(testCtx(t), a.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("idle pooled connection stayed open")
	}
	ae, ok := errors.AsType[*quic.ApplicationError](context.Cause(c.qc.Context()))
	if !ok || ae.ErrorCode != 0 || ae.ErrorMessage != "idle" {
		t.Fatalf("close reason %v", context.Cause(c.qc.Context()))
	}
}

func TestLimiterBlocksAfterFailures(t *testing.T) {
	g := newGate()
	ip := func(s string) source { return addrSource(net.UDPAddrFromAddrPort(netip.MustParseAddrPort(s))) }
	for range maxFailures {
		if g.blocked(ip("192.0.2.1:1")) {
			t.Fatal("blocked too early")
		}
		g.fail(ip("192.0.2.1:1"))
	}
	if !g.blocked(ip("192.0.2.1:9")) || g.blocked(ip("192.0.2.2:1")) {
		t.Fatal("IPv4 addresses do not count apart")
	}
	if ip("[2001:db8::1]:1") != ip("[2001:db8::2]:1") || ip("[2001:db8:0:1::1]:1") == ip("[2001:db8::1]:1") {
		t.Fatal("IPv6 sources do not count per /64")
	}
	if ip("[::ffff:192.0.2.1]:1") != ip("192.0.2.1:1") {
		t.Fatal("IPv4-mapped does not count as IPv4")
	}
	g.succeed(ip("192.0.2.1:1"))
	if g.blocked(ip("192.0.2.1:1")) {
		t.Fatal("success did not forget failures")
	}
	for i := range globalFailures {
		g.fail(source{node: mesh.NodeID{byte(i), byte(i >> 8)}})
	}
	if !g.blocked(ip("198.51.100.1:1")) {
		t.Fatal("global budget not enforced")
	}
}

func TestWaysEndWithRelay(t *testing.T) {
	n := start(t, Config{})
	for _, w := range n.Ways() {
		if w.Relay {
			t.Fatal("relay way without a relay")
		}
		ip := netip.MustParseAddr(w.Host)
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || w.Port != uint16(n.port) {
			t.Fatalf("bad own way %v", w)
		}
	}
	r := relay.New("http://127.0.0.1:1", n.key)
	m := start(t, Config{Relay: r, Port: -1})
	if ways := m.Ways(); len(ways) != 1 || !ways[0].Relay {
		t.Fatalf("relay-only node ways %v", ways)
	}
}

// runRelay builds and runs stencil's relay on a free loopback port; its URL.
// OVNFS_RELAY_SRC names the relay's source directory; unset skips the test.
func runRelay(t *testing.T) string {
	t.Helper()
	relaySource := os.Getenv("OVNFS_RELAY_SRC")
	if relaySource == "" {
		t.Skip("OVNFS_RELAY_SRC not set to the relay's source directory")
	}
	if _, err := os.Stat(filepath.Join(relaySource, "node.go")); err != nil {
		t.Skipf("node relay source not available (missing, or a checkout older than the /node route): %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "relay")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = relaySource
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the relay: %v\n%s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bind := ln.Addr().String()
	ln.Close()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "RELAY_BIND="+bind, "SHARE_DIR="+filepath.Join(dir, "share"), "RELAY_CH_DSN=")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", bind, time.Second)
		if err == nil {
			c.Close()
			return "http://" + bind
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay not listening on %s: %v", bind, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// relayNode runs a relay-only node (no direct socket) linked to url.
func relayNode(t *testing.T, url string, cfg Config) *Node {
	t.Helper()
	key, err := mesh.LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := relay.New(url, key)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		r.PacketConn().Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	for !r.Up() {
		if time.Now().After(deadline) {
			t.Fatal("relay link never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cfg.Key, cfg.Relay, cfg.Port = key, r, -1
	return start(t, cfg)
}

func TestConnectThroughRelay(t *testing.T) {
	url := runRelay(t)
	a := relayNode(t, url, Config{ALPNs: []string{"test"}})
	l, _ := a.Listen("test")
	accepted := serve(t, l)
	b := relayNode(t, url, Config{Book: &book{}})
	c, err := b.Connect(testCtx(t), a.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Peer(); !p.Relayed || !p.Way.Relay || p.Node != a.ID() || !relay.IsRelayed(p.Addr) {
		t.Fatalf("dialed peer %+v", p)
	}
	for i := range 3 {
		roundTrip(t, c, "relayed "+strconv.Itoa(i))
	}
	ac := <-accepted
	if p := ac.Peer(); !p.Relayed || p.Node != b.ID() {
		t.Fatalf("accepted peer %+v", p)
	}
	e1, _ := c.Exporter("EXPORTER-test")
	e2, _ := ac.Exporter("EXPORTER-test")
	if e1 != e2 {
		t.Fatal("exporters differ over the relay")
	}
	// A node the relay does not know is unreachable, not hung.
	stranger, _ := mesh.LoadOrCreateKey(t.TempDir())
	if _, err := b.Connect(testCtx(t), stranger.ID(), "test"); !errors.Is(err, relay.ErrNoPeer) {
		t.Fatalf("unknown node over the relay: %v", err)
	}
}
