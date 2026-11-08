package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// linkSocket is where `ovnfs link` pipes an incoming ssh session into the daemon.
const linkSocket = "link.sock"

// nodeFile holds this daemon's node id under its state directory.
const nodeFile = "node_id"

// links runs both ends of attach: outbound links this daemon serves as a source
// (one ssh session per attached checkout and machine, reconnected until
// stopped), and inbound sessions other machines opened to attach checkouts here.
type links struct {
	reg      *checkout.Registry
	emit     func(checkout.Event) // admin events, such as push progress; nil drops them
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	sessions map[string]*peer.Session // link key → current inbound session
	out      map[string]*outLink      // "<checkout>@<host>" → running outbound link
	wg       sync.WaitGroup
}

// newLinks prepares an idle link manager publishing events to emit (nil drops
// them); the registry is set once it is open.
func newLinks(emit func(checkout.Event)) *links {
	ctx, cancel := context.WithCancel(context.Background())
	return &links{emit: emit, ctx: ctx, cancel: cancel, sessions: map[string]*peer.Session{}, out: map[string]*outLink{}}
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

// listen accepts link connections `ovnfs link` piped to <state>/link.sock for this
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
			l.inbound(conn)
		})
	}
}

// inbound attaches the offered checkout and serves its reads for the session's life.
func (l *links) inbound(conn io.ReadWriteCloser) {
	s, offer, err := peer.Accept(conn, l.reg.Store())
	if err != nil {
		log.Printf("link: %v", err)
		conn.Close()
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
	s.SetPushHandler(func() []peer.Want { return peerWants(l.reg.Missing(key)) })
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
	o := newOutLink(l.ctx, name, args.To, cmp.Or(args.RemoteBin, "ovnfs"), offer, blobs)
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
	found := l.attached(args.Checkout, args.From)
	if len(found) == 0 {
		return api.PullReply{}, &checkout.Error{Code: "not_found", Message: fmt.Sprintf("checkout %s has no matching attachment; attach it first", args.Checkout)}
	}
	if len(found) > 1 {
		hosts := make([]string, len(found))
		for i, o := range found {
			hosts[i] = o.to
		}
		return api.PullReply{}, &checkout.Error{Code: "invalid", Message: fmt.Sprintf("checkout %s is attached to %s; choose --from HOST", args.Checkout, strings.Join(hosts, ", "))}
	}
	selected := found[0]
	p := selected.remote()
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

// push has the machines of the matching attachments fetch everything they
// still read through their links, all at once; one attachment failing does
// not stop the others, and the reply says how each went. Progress goes out as
// "pushing" events carrying an api.Pushed.
func (l *links) push(ctx context.Context, args api.PushArgs) (api.PushReply, error) {
	found := l.attached(args.Checkout, args.To)
	if len(found) == 0 {
		what := "no checkout is attached"
		if args.Checkout != "" {
			what = "checkout " + args.Checkout + " is not attached"
		}
		if args.To != "" {
			what += " to " + args.To
		}
		return api.PushReply{}, &checkout.Error{Code: "not_found", Message: what + "; attach it first"}
	}
	reply := api.PushReply{Pushed: make([]api.Pushed, len(found))}
	var wg sync.WaitGroup
	progress := func(p api.Pushed) {
		if l.emit != nil {
			l.emit(checkout.Event{Name: "pushing", Payload: p})
		}
	}
	for i, o := range found {
		wg.Go(func() { reply.Pushed[i] = o.push(ctx, progress) })
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return api.PushReply{}, err
	}
	return reply, nil
}

// attached returns the outbound links of the checkout name to host, sorted by
// checkout and then host; "" matches every checkout or host.
func (l *links) attached(name, host string) []*outLink {
	l.mu.Lock()
	var found []*outLink
	for _, o := range l.out {
		if (name == "" || o.checkout == name) && (host == "" || o.to == host) {
			found = append(found, o)
		}
	}
	l.mu.Unlock()
	slices.SortFunc(found, func(a, b *outLink) int {
		return cmp.Or(strings.Compare(a.checkout, b.checkout), strings.Compare(a.to, b.to))
	})
	return found
}

// captureDir opens (or refreshes) the checkout that stands for a folder being attached:
// a clean checkout of the same folder is replaced so the offer shows the folder as it is now.
func (l *links) captureDir(ctx context.Context, dir string) (string, error) {
	root, err := source.Resolve(dir)
	if err != nil {
		return "", &checkout.Error{Code: "not_found", Message: err.Error()}
	}
	ls, err := l.reg.Ls(ctx, api.LsArgs{})
	if err != nil {
		return "", err
	}
	for _, h := range ls.Checkouts {
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

// outLink is one attached checkout served to one machine.
type outLink struct {
	mu       sync.Mutex
	active   *peer.Remote // nil while disconnected
	ctx      context.Context
	cancel   context.CancelFunc
	checkout string // the local checkout offered
	to       string // ssh destination
	bin      string // ovnfs on the machine
	offer    peer.Offer
	blobs    peer.Blobs
	accepted chan peer.Accepted // the first acceptance only
	failed   chan error         // the first attempt's failure only
}

// newOutLink prepares a link offering the checkout name to the machine to,
// whose ovnfs is bin; run starts it.
func newOutLink(parent context.Context, name, to, bin string, offer peer.Offer, blobs peer.Blobs) *outLink {
	ctx, cancel := context.WithCancel(parent)
	return &outLink{ctx: ctx, cancel: cancel, checkout: name, to: to, bin: bin, offer: offer, blobs: blobs, accepted: make(chan peer.Accepted, 1), failed: make(chan error, 1)}
}

// remote is the connected session's handle, nil while the link is down.
func (o *outLink) remote() *peer.Remote {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.active
}

// push has the machine fetch everything it still reads through this link,
// passing on its progress; a failed push reports how far it got.
func (o *outLink) push(ctx context.Context, progress func(api.Pushed)) api.Pushed {
	r := api.Pushed{Checkout: o.checkout, To: o.to}
	p := o.remote()
	if p == nil {
		r.Error = "not connected; the link is reconnecting"
		return r
	}
	var mu sync.Mutex // progress may still run after a canceled Push returns
	set := func(at peer.Pushed) api.Pushed {
		mu.Lock()
		defer mu.Unlock()
		r.Blobs, r.Bytes, r.TotalBlobs, r.TotalBytes = at.Blobs, at.Bytes, at.TotalBlobs, at.TotalBytes
		return r
	}
	pushed, err := p.Push(ctx, func(at peer.Pushed) { progress(set(at)) })
	if err == nil {
		set(pushed)
	}
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		r.Error = err.Error()
	}
	return r
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
	conn, err := sshOpen(o.ctx, o.to, o.bin)
	if err != nil {
		return err
	}
	defer func() {
		o.mu.Lock()
		o.active = nil
		o.mu.Unlock()
	}()
	err = peer.ServeRemote(o.ctx, conn, o.offer, o.blobs, func(a peer.Accepted, p *peer.Remote) {
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

// sshOpen runs `bin link` on host over ssh, whose stdio pipes into that
// machine's daemon, starting it if needed.
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
	closed sync.Once
	err    error // what Close returns
}

// Close closes the child's stdin and stops it; what ssh printed comes back as
// the error, to explain a failed session. The link closes its connection from
// several goroutines: later calls wait for the first and return its result,
// since concurrent cmd.Wait calls would block forever.
func (c *sshConn) Close() error {
	c.closed.Do(func() {
		c.WriteCloser.Close()
		c.cmd.Process.Kill()
		c.cmd.Wait()
		if s := c.stderr.String(); s != "" {
			c.err = errors.New(s)
		}
	})
	return c.err
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

// loadNode returns this daemon's node id, creating a random one on first start.
// Its offers carry the id so the machines they reach tell its sources apart
// from other daemons' (see checkout.LinkKey).
func loadNode(state string) (string, error) {
	path := filepath.Join(state, nodeFile)
	b, err := os.ReadFile(path)
	switch {
	case err == nil && len(b) > 0:
		return string(b), nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	id := rand.Text()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return id, nil
}
