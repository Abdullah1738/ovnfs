package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/peer"
)

// pipedLink is the source end of a link piped into a links manager, as an
// outbound link's ssh session is.
type pipedLink struct {
	offer  peer.Offer
	blobs  peer.Blobs
	remote *peer.Remote
	stop   context.CancelFunc
	ended  chan error    // what serving the link returned
	gone   chan struct{} // closed once the manager dropped the session
}

// pipeLink offers a checkout to host over net.Pipe and waits for the acceptance.
func pipeLink(t *testing.T, host *links, offer peer.Offer, blobs peer.Blobs) *pipedLink {
	t.Helper()
	left, right := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &pipedLink{offer: offer, blobs: blobs, stop: cancel, ended: make(chan error, 1), gone: make(chan struct{})}
	host.wg.Go(func() {
		defer close(p.gone)
		host.inbound(right)
	})
	accepted := make(chan peer.Accepted, 1)
	go func() {
		p.ended <- peer.ServeRemote(ctx, left, offer, blobs, func(a peer.Accepted, r *peer.Remote) {
			p.remote = r
			accepted <- a
		})
	}()
	select {
	case a := <-accepted:
		if a.Checkout != offer.Name {
			t.Fatalf("offer of %s attached as %s", offer.Name, a.Checkout)
		}
	case err := <-p.ended:
		t.Fatalf("link of %s: %v", offer.Name, err)
	}
	return p
}

// TestCheckoutsOfOneFolderKeepTheirLinks: two checkouts of one folder attached
// from one daemon each hold their own session instead of closing the other's,
// reads of that folder go through whichever is up, and a checkout connecting
// again replaces only its own stale session.
func TestCheckoutsOfOneFolderKeepTheirLinks(t *testing.T) {
	open := func(c checkout.Config) *checkout.Registry {
		root := t.TempDir()
		c.State, c.Mount = filepath.Join(root, "state"), filepath.Join(root, "mount")
		c.Memory, c.BlobCache = overlay.NewMemory(64<<20, 1<<20, 0), 64<<20
		reg, err := checkout.Open(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { reg.Close() })
		return reg
	}
	src := open(checkout.Config{Node: "n_src"})
	host := newLinks(nil)
	host.reg = open(checkout.Config{Fetch: host.fetch, Search: host.search})
	t.Cleanup(host.close)

	ctx := context.Background()
	dir := t.TempDir()
	for name, data := range map[string]string{"one.txt": "one", "two.txt": "two"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	attach := func(name string) *pipedLink {
		if _, err := src.New(ctx, api.NewArgs{Dir: dir, Name: name}); err != nil {
			t.Fatal(err)
		}
		offer, blobs, _, err := src.Offer(ctx, name, "")
		if err != nil {
			t.Fatal(err)
		}
		return pipeLink(t, host, offer, blobs)
	}
	a, b := attach("a"), attach("b")
	key := checkout.LinkKey(a.offer.Node, a.offer.Source)
	wants := host.reg.Missing(key)
	if len(wants) != 2 {
		t.Fatalf("missing %v, want the folder's two files", wants)
	}

	b.stop()
	<-b.gone
	if err := host.fetch(ctx, key, wants[:1]); err != nil {
		t.Fatalf("read through a's link once b's ended: %v", err)
	}

	b = pipeLink(t, host, b.offer, b.blobs)
	if pushed, err := a.remote.Push(ctx, nil); err != nil || pushed.Blobs != 1 {
		t.Fatalf("push through a's link once b connected again: %+v, %v", pushed, err)
	}
	if left := host.reg.Missing(key); len(left) != 0 {
		t.Fatalf("still missing after the push: %v", left)
	}

	// The host never saw a's connection end: connecting again closes it.
	again := pipeLink(t, host, a.offer, a.blobs)
	select {
	case err := <-a.ended:
		if err != nil {
			t.Fatalf("a's stale link ended with %v, want a clean close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a's stale session stayed open after a connected again")
	}
	for name, p := range map[string]*pipedLink{"a": again, "b": b} {
		if _, err := p.remote.Push(ctx, nil); err != nil {
			t.Fatalf("push through %s's link after a connected again: %v", name, err)
		}
	}
}
