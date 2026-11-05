package relay

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/mesh"
)

// testRelay is a relay process on a loopback port, restartable on the same
// port.
type testRelay struct {
	t     *testing.T
	bin   string
	bind  string
	share string
	cmd   *exec.Cmd
}

// startRelay builds stencil's relay and runs it on a free loopback port.
// OVNFS_RELAY_SRC names the relay's source directory; unset skips the test.
func startRelay(t *testing.T) *testRelay {
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
	r := &testRelay{t: t, bin: bin, bind: bind, share: filepath.Join(dir, "share")}
	r.start()
	t.Cleanup(r.stop)
	return r
}

func (r *testRelay) url() string { return "http://" + r.bind }

func (r *testRelay) start() {
	r.t.Helper()
	cmd := exec.Command(r.bin)
	cmd.Env = append(os.Environ(), "RELAY_BIND="+r.bind, "SHARE_DIR="+r.share, "RELAY_CH_DSN=")
	if testing.Verbose() {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.cmd = cmd
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", r.bind, time.Second)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("relay not listening on %s: %v", r.bind, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop ends the relay as a deployment would (SIGTERM: links get closing
// "restarting") and waits for it.
func (r *testRelay) stop() {
	if r.cmd == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = r.cmd.Process.Kill()
	} else {
		_ = r.cmd.Process.Signal(os.Interrupt)
	}
	done := make(chan struct{})
	go func() { _ = r.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = r.cmd.Process.Kill()
		<-done
	}
	r.cmd = nil
}

// newClient runs a client with a fresh key against url until the test ends.
func newClient(t *testing.T, url string) *Client {
	t.Helper()
	key, err := mesh.LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := New(url, key)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		c.PacketConn().Close()
	})
	return c
}

// linkedAfter waits until c has a link up numbered above n; that number.
func linkedAfter(t *testing.T, c *Client, n uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		up, links := c.up, c.links
		c.mu.Unlock()
		if up && links > n && c.welcomed.Load() {
			return links
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("link %s not up after link %d", c.id.Short(), n)
	return 0
}

// exchange has from send payload to to's synthetic address until to reads
// it (relayed datagrams may be dropped while routes settle); the address
// to read it from.
func exchange(t *testing.T, from, to *Client, toAddr net.Addr, payload []byte) net.Addr {
	t.Helper()
	pc := to.PacketConn()
	buf := make([]byte, 2048)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := from.PacketConn().WriteTo(payload, toAddr); err != nil {
			t.Fatalf("WriteTo: %v", err)
		}
		pc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					t.Fatalf("ReadFrom: %v", err)
				}
				break
			}
			if bytes.Equal(buf[:n], payload) {
				pc.SetReadDeadline(time.Time{})
				return addr
			}
		}
	}
	t.Fatalf("%q never arrived", payload)
	return nil
}

// roundTrip shows a and b exchanging datagrams both ways through the relay,
// each seeing the other behind a stable synthetic address.
func roundTrip(t *testing.T, a, b *Client, tag string) {
	t.Helper()
	aSeen := exchange(t, a, b, a.AddrOf(b.id), []byte("ping "+tag))
	if !IsRelayed(aSeen) {
		t.Fatalf("b reads a from %v, not a synthetic address", aSeen)
	}
	if peer, ok := b.PeerOf(aSeen); !ok || peer != a.id {
		t.Fatalf("b reads a from %v, which it maps to %v %v", aSeen, peer.Short(), ok)
	}
	bSeen := exchange(t, b, a, aSeen, []byte("pong "+tag))
	if bSeen.String() != a.AddrOf(b.id).String() {
		t.Fatalf("a reads b from %v, want %v", bSeen, a.AddrOf(b.id))
	}
}

// TestRelayRoundTripAndRestart: two nodes authenticate to stencil's relay,
// exchange datagrams both ways through their PacketConns, and do so again
// once the relay process restarted.
func TestRelayRoundTripAndRestart(t *testing.T) {
	r := startRelay(t)
	a, b := newClient(t, r.url()), newClient(t, r.url())
	aLink, bLink := linkedAfter(t, a, 0), linkedAfter(t, b, 0)

	if !a.Up() || !b.Up() {
		t.Fatal("links not up")
	}
	if obs, ok := a.Observed(); !ok || !obs.Addr().IsLoopback() {
		t.Fatalf("observed = %v %v, want a loopback address", obs, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Reaching(ctx, b.id); err != nil {
		t.Fatalf("Reaching b: %v", err)
	}
	roundTrip(t, a, b, "before restart")

	r.stop()
	r.start()
	linkedAfter(t, a, aLink)
	linkedAfter(t, b, bLink)
	roundTrip(t, a, b, "after restart")
}

// TestRelayV1WaitsForRoute: on a v1 link a datagram to a peer without a
// route waits for the route, then goes, and the v2 peer answers it.
func TestRelayV1WaitsForRoute(t *testing.T) {
	r := startRelay(t)
	key, err := mesh.LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := New(r.url(), key)
	a.offer = []string{protocolV1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	b := newClient(t, r.url())
	linkedAfter(t, a, 0)
	linkedAfter(t, b, 0)
	a.mu.Lock()
	v2 := a.out.v2
	a.mu.Unlock()
	if v2 {
		t.Fatal("a v1-only offer got a v2 link")
	}

	// One write, no retries: the datagram must wait for the route.
	if _, err := a.PacketConn().WriteTo([]byte("first"), a.AddrOf(b.id)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	b.PacketConn().SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := b.PacketConn().ReadFrom(buf)
	if err != nil || string(buf[:n]) != "first" {
		t.Fatalf("b read %q, %v", buf[:n], err)
	}
	roundTrip(t, b, a, "v1")
}

// TestRelayNoPeer: asking for a node the relay has no link to fails with
// ErrNoPeer.
func TestRelayNoPeer(t *testing.T) {
	r := startRelay(t)
	a := newClient(t, r.url())
	linkedAfter(t, a, 0)
	absent, err := mesh.LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Reaching(ctx, absent.ID()); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("Reaching an absent node: %v, want ErrNoPeer", err)
	}
}

// TestRelayLive exchanges a datagram between two nodes through the
// production relay (OVNFS_RELAY_LIVE=1).
func TestRelayLive(t *testing.T) {
	if os.Getenv("OVNFS_RELAY_LIVE") != "1" {
		t.Skip("set OVNFS_RELAY_LIVE=1 to use " + Production)
	}
	a, b := newClient(t, Production), newClient(t, Production)
	linkedAfter(t, a, 0)
	linkedAfter(t, b, 0)
	a.mu.Lock()
	v2 := a.out.v2
	a.mu.Unlock()
	if !v2 {
		t.Error("production relay did not choose stencil-relay.v2")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Reaching(ctx, b.id); err != nil {
		t.Fatalf("Reaching b: %v", err)
	}
	roundTrip(t, a, b, "live")
}

// TestQueueShedsStaleDatagrams: datagrams that waited past the sojourn are
// dropped when the writer takes the queue; control frames never are.
func TestQueueShedsStaleDatagrams(t *testing.T) {
	q := outQueue{live: true}
	t0 := time.Now()
	q.push(&frame{kind: kindSend, route: 1, datagram: []byte("old")}, t0)
	q.push(&frame{kind: kindConnect}, t0)
	q.push(&frame{kind: kindSend, route: 1, datagram: []byte("new")}, t0.Add(sojourn))
	buf, ends := q.take(t0.Add(sojourn+time.Millisecond), nil, nil)
	want := "connect peer=" + strings.Repeat("0", 64) + "; send route=1 datagram=" + hex.EncodeToString([]byte("new"))
	if got := readText(buf); got != want {
		t.Fatalf("taken: %s", got)
	}
	if len(ends) != 2 || ends[1] != len(buf) {
		t.Fatalf("ends %v for %d bytes", ends, len(buf))
	}
	q = outQueue{}
	if q.push(&frame{kind: kindPing}, t0) {
		t.Fatal("a queue without a link took a frame")
	}
}

// TestSyntheticAddresses: synthetic addresses are distinct, in 240/8 on
// port 1, and only they read as relayed.
func TestSyntheticAddresses(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range []uint32{1, 2, 0xffff, 0xffffff} {
		a := net.UDPAddrFromAddrPort(synthetic(n))
		if !IsRelayed(a) || a.Port != 1 || seen[a.String()] {
			t.Fatalf("synthetic(%d) = %v", n, a)
		}
		seen[a.String()] = true
		mapped := &net.UDPAddr{IP: a.IP.To16(), Port: a.Port}
		if !IsRelayed(mapped) {
			t.Fatalf("%v (IPv4-mapped) not relayed", mapped)
		}
	}
	for _, real := range []string{"192.168.1.69:8377", "[fd7a:115c:a1e0::5]:1", "[::1]:1", "127.0.0.1:1"} {
		a, _ := net.ResolveUDPAddr("udp", real)
		if IsRelayed(a) {
			t.Fatalf("%v reads as relayed", a)
		}
	}
}
