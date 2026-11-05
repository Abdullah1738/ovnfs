package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/internal/search/grep"
	"github.com/stencil-hq/ovnfs/peer"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// linkSocket is where `ovnfs link` relays an incoming ssh session into the daemon.
const linkSocket = "link.sock"

// links runs both ends of attach: outbound links this daemon serves as a source
// (one session per attached checkout and machine, over the mesh or ssh, reconnected
// until stopped), and inbound sessions other machines opened to attach checkouts here.
type links struct {
	reg      *checkout.Registry
	mesh     *meshd // set once the mesh is open
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	sessions map[string]*peer.Session // link key → current inbound session
	out      map[string]*outLink      // "<checkout>@<host>" → running outbound link
	wg       sync.WaitGroup
}

// newLinks prepares an idle link manager; the registry is set once it is open.
func newLinks() *links {
	ctx, cancel := context.WithCancel(context.Background())
	return &links{ctx: ctx, cancel: cancel, sessions: map[string]*peer.Session{}, out: map[string]*outLink{}}
}

// fetch serves checkout.Config.Fetch: it asks the session connected for key.
func (l *links) fetch(ctx context.Context, key string, wants []base.Want) error {
	s, err := l.session(key)
	if err != nil {
		return err
	}
	return s.Fetch(ctx, peerWants(wants))
}

// search serves checkout.Config.Search: it asks the session connected for key.
func (l *links) search(ctx context.Context, key string, q grep.Query, wants []base.Want) ([]grep.Outcome, error) {
	s, err := l.session(key)
	if err != nil {
		return nil, err
	}
	return s.Search(ctx, q, peerWants(wants))
}

// session returns the inbound session connected for key.
func (l *links) session(key string) (*peer.Session, error) {
	l.mu.Lock()
	s := l.sessions[key]
	l.mu.Unlock()
	if s == nil {
		_, src, _ := strings.Cut(key, " ")
		return nil, fmt.Errorf("%s is not connected; run `ovnfs attach` there again", src)
	}
	return s, nil
}

// peerWants converts a base's wants to the link protocol's.
func peerWants(wants []base.Want) []peer.Want {
	pw := make([]peer.Want, len(wants))
	for i, w := range wants {
		pw[i] = peer.Want{ID: w.ID, Path: w.Path, Size: w.Size}
	}
	return pw
}

// listen accepts link connections ssh relayed to <state>/link.sock by this
// daemon's user until the listener closes.
func (l *links) listen(ln net.Listener) {
	l.wg.Add(1)
	defer l.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		l.wg.Go(func() {
			if err := admin.CheckPeer(conn); err != nil {
				log.Printf("link: %v", err)
				conn.Close()
				return
			}
			l.inbound(conn, "")
		})
	}
}

// inbound attaches the offered checkout and serves its reads for the session's life.
// from is the node a mesh connection proved ("" over ssh, which proves nothing);
// an offer must then come from that node.
func (l *links) inbound(conn io.ReadWriteCloser, from string) {
	s, offer, err := peer.Accept(conn, l.reg.Store())
	if err != nil {
		log.Printf("link: %v", err)
		conn.Close()
		return
	}
	if from != "" && offer.Node != from {
		log.Printf("link from %s: offer claims node %s", from, offer.Node)
		s.Refuse(fmt.Errorf("the offer names node %s but the link comes from %s", offer.Node, from))
		return
	}
	h, err := l.reg.AttachRemote(l.ctx, offer)
	if err != nil {
		log.Printf("link from %s: %v", offer.Source, err)
		s.Refuse(err)
		return
	}
	key := checkout.LinkKey(offer.Node, offer.Source)
	l.mu.Lock()
	old := l.sessions[key]
	l.sessions[key] = s
	l.mu.Unlock()
	if old != nil {
		old.Close()
	}
	s.SetPullHandler(func(ctx context.Context, _ store.OID) (peer.Pulled, error) {
		return l.reg.RemoteChanges(ctx, h.Name, offer.Tree)
	})
	host, _ := os.Hostname()
	if err := s.Reply(peer.Accepted{Checkout: h.Name, Path: h.Path, Host: host}); err != nil {
		log.Printf("link from %s: %v", offer.Source, err)
	} else {
		log.Printf("link from %s attached as %s", offer.Source, h.Name)
		select {
		case <-s.Done():
		case <-l.ctx.Done():
		}
	}
	s.Close()
	l.mu.Lock()
	if l.sessions[key] == s {
		delete(l.sessions, key)
	}
	l.mu.Unlock()
	log.Printf("link from %s ended: %v", offer.Source, s.Err())
}

// attach offers a checkout (or a folder captured now) to a machine and keeps the link up.
func (l *links) attach(ctx context.Context, args api.AttachArgs) (api.AttachReply, error) {
	if args.To == "" {
		return api.AttachReply{}, &checkout.Error{Code: "invalid", Message: "attach needs --to HOST"}
	}
	name := args.Checkout
	if (args.Dir == "") == (name == "") {
		return api.AttachReply{}, &checkout.Error{Code: "invalid", Message: "attach needs exactly one of a folder or a checkout"}
	}
	if args.Dir != "" {
		var err error
		if name, err = l.captureDir(ctx, args.Dir); err != nil {
			return api.AttachReply{}, err
		}
	}
	offer, blobs, snap, err := l.reg.Offer(ctx, name, args.Name)
	if err != nil {
		return api.AttachReply{}, err
	}
	key := name + "@" + args.To
	l.mu.Lock()
	if old := l.out[key]; old != nil {
		old.cancel()
	}
	o := newOutLink(l.ctx, args.To, l.opener(args.To, args.RemoteBin), offer, blobs)
	l.out[key] = o
	l.mu.Unlock()
	l.wg.Go(func() {
		o.run()
		l.mu.Lock()
		if l.out[key] == o {
			delete(l.out, key)
		}
		l.mu.Unlock()
	})
	select {
	case a := <-o.accepted:
		return api.AttachReply{Checkout: name, Snapshot: snap, Host: a.Host, RemoteCheckout: a.Checkout, RemotePath: a.Path}, nil
	case err := <-o.failed:
		return api.AttachReply{}, &checkout.Error{Code: "unavailable", Message: err.Error()}
	case <-ctx.Done():
		o.cancel()
		return api.AttachReply{}, ctx.Err()
	}
}

// pull retrieves one attachment's source changes and merges them into its local checkout.
func (l *links) pull(ctx context.Context, args api.PullArgs) (api.PullReply, error) {
	if args.Checkout == "" {
		return api.PullReply{}, &checkout.Error{Code: "invalid", Message: "pull needs a checkout"}
	}
	l.mu.Lock()
	var selected *outLink
	var hosts []string
	for key, o := range l.out {
		host, ok := strings.CutPrefix(key, args.Checkout+"@")
		if !ok || (args.From != "" && host != args.From) {
			continue
		}
		selected = o
		hosts = append(hosts, host)
	}
	l.mu.Unlock()
	if len(hosts) == 0 {
		return api.PullReply{}, &checkout.Error{Code: "not_found", Message: fmt.Sprintf("checkout %s has no matching attachment; attach it first", args.Checkout)}
	}
	if len(hosts) > 1 {
		sort.Strings(hosts)
		return api.PullReply{}, &checkout.Error{Code: "invalid", Message: fmt.Sprintf("checkout %s is attached to %s; choose --from HOST", args.Checkout, strings.Join(hosts, ", "))}
	}
	selected.mu.Lock()
	p := selected.active
	selected.mu.Unlock()
	if p == nil {
		return api.PullReply{}, &checkout.Error{Code: "unavailable", Message: fmt.Sprintf("attachment %s@%s is not connected", args.Checkout, selected.to)}
	}
	pulled, err := p.Pull(ctx, selected.offer.Tree)
	if err != nil {
		if ctx.Err() != nil {
			return api.PullReply{}, ctx.Err()
		}
		return api.PullReply{}, &checkout.Error{Code: "unavailable", Message: fmt.Sprintf("pull from %s: %v", selected.to, err)}
	}
	return l.reg.MergePull(ctx, args, selected.offer.Tree, pulled)
}

// captureDir opens (or refreshes) the checkout that stands for a folder being attached:
// a clean checkout of the same folder is replaced so the offer shows the folder as it is now.
func (l *links) captureDir(ctx context.Context, dir string) (string, error) {
	root, err := source.Resolve(dir)
	if err != nil {
		return "", &checkout.Error{Code: "not_found", Message: err.Error()}
	}
	for _, h := range l.reg.Ls().Checkouts {
		if h.Source != root {
			continue
		}
		if h.Dirty {
			return "", &checkout.Error{Code: "exists", Message: fmt.Sprintf("checkout %s of %s has unsnapshotted changes; attach it by name instead", h.Name, root)}
		}
		if err := l.reg.Rm(ctx, api.RmArgs{Checkout: h.Name}); err != nil {
			return "", err
		}
	}
	reply, err := l.reg.New(ctx, api.NewArgs{Dir: root})
	if err != nil {
		return "", err
	}
	return reply.Checkout.Name, nil
}

// close stops every link and waits for them.
func (l *links) close() {
	l.cancel()
	l.mu.Lock()
	for _, s := range l.sessions {
		s.Close()
	}
	l.mu.Unlock()
	l.wg.Wait()
}

// opener starts one session's connection to the machine a link serves.
type opener func(ctx context.Context) (io.ReadWriteCloser, error)

// dialTimeout bounds reaching a member before a session starts.
const dialTimeout = 30 * time.Second

// opener picks the link's transport: the mesh when to names a member, else ssh
// to run bin (default ovnfs) there.
func (l *links) opener(to, bin string) opener {
	if l.mesh != nil {
		if mem, err := l.mesh.book.Find(to); err == nil {
			return func(ctx context.Context) (io.ReadWriteCloser, error) {
				dctx, cancel := context.WithTimeout(ctx, dialTimeout)
				defer cancel()
				return l.mesh.openLink(dctx, mem.Node)
			}
		}
	}
	if bin == "" {
		bin = "ovnfs"
	}
	return func(ctx context.Context) (io.ReadWriteCloser, error) { return sshOpen(ctx, to, bin) }
}

// outLink is one attached checkout served to one machine.
type outLink struct {
	mu       sync.Mutex
	active   *peer.Puller
	ctx      context.Context
	cancel   context.CancelFunc
	to       string
	open     opener
	offer    peer.Offer
	blobs    peer.Blobs
	accepted chan peer.Accepted // the first acceptance only
	failed   chan error         // the first attempt's failure only
}

// newOutLink prepares a link; run starts it.
func newOutLink(parent context.Context, to string, open opener, offer peer.Offer, blobs peer.Blobs) *outLink {
	ctx, cancel := context.WithCancel(parent)
	return &outLink{ctx: ctx, cancel: cancel, to: to, open: open, offer: offer, blobs: blobs, accepted: make(chan peer.Accepted, 1), failed: make(chan error, 1)}
}

// run keeps the session up, reconnecting with backoff after the first acceptance.
func (o *outLink) run() {
	delay := time.Second
	connected := false
	for o.ctx.Err() == nil {
		err := o.session(func(a peer.Accepted) {
			if !connected {
				connected = true
				o.accepted <- a
			}
			delay = time.Second
		})
		if o.ctx.Err() != nil {
			return
		}
		if !connected {
			o.failed <- fmt.Errorf("attach to %s: %w", o.to, err)
			return
		}
		log.Printf("link %s to %s ended: %v; reconnecting in %s", o.offer.Name, o.to, err, delay)
		select {
		case <-o.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

// session opens one connection to the machine and serves the offer over it.
func (o *outLink) session(accepted func(peer.Accepted)) error {
	conn, err := o.open(o.ctx)
	if err != nil {
		return err
	}
	defer func() {
		o.mu.Lock()
		o.active = nil
		o.mu.Unlock()
	}()
	err = peer.ServePull(o.ctx, conn, o.offer, o.blobs, func(a peer.Accepted, p *peer.Puller) {
		o.mu.Lock()
		o.active = p
		o.mu.Unlock()
		accepted(a)
	})
	detail := conn.Close()
	switch {
	case err == nil && detail == nil:
		return errors.New("remote closed the link")
	case err == nil:
		return detail
	case detail != nil:
		return fmt.Errorf("%w: %v", err, detail)
	}
	return err
}

// sshOpen runs `bin link` on host over ssh, whose stdio relays into that
// machine's daemon.
func sshOpen(ctx context.Context, host, bin string) (io.ReadWriteCloser, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", host, bin, "link")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &sshConn{Reader: stdout, WriteCloser: stdin, cmd: cmd}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return c, nil
}

// sshConn is an ssh child's stdio as one stream; closing it ends the child.
type sshConn struct {
	io.Reader
	io.WriteCloser
	cmd    *exec.Cmd
	stderr tailBuffer
}

// Close closes the child's stdin and stops it; what ssh printed comes back as
// the error, to explain a failed session.
func (c *sshConn) Close() error {
	c.WriteCloser.Close()
	c.cmd.Process.Kill()
	c.cmd.Wait()
	if s := c.stderr.String(); s != "" {
		return errors.New(s)
	}
	return nil
}

// tailBuffer keeps the last few KB a child wrote to stderr, for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

// Write appends p, keeping only the tail.
func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > 4096 {
		b.buf = b.buf[len(b.buf)-4096:]
	}
	return len(p), nil
}

// String returns the kept tail, trimmed.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// linkListen opens the daemon's private link socket, replacing only a stale one.
func linkListen(state string) (net.Listener, error) {
	return admin.Listen(filepath.Join(state, linkSocket))
}
