package peer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/store"
)

// Blobs serves content on the source side. path is the leaf path the id was offered under (a hint for finding the object in a repository).
type Blobs interface {
	Open(id store.OID, path string) (io.ReadCloser, int64, error)
}

// Serve runs the source side: sends offer, waits for Accepted (calling accepted once) or a refusal (returned as error), then answers blob requests until the stream ends or ctx is done. Returns nil on a clean close by the remote.
// Serve owns conn and closes it before returning; on ctx done it says goodbye to the remote first and returns ctx's error.
func Serve(ctx context.Context, conn io.ReadWriteCloser, offer Offer, blobs Blobs, accepted func(Accepted)) error {
	return ServeRemote(ctx, conn, offer, blobs, func(a Accepted, _ *Remote) {
		if accepted != nil {
			accepted(a)
		}
	})
}

// ServeRemote serves blobs like Serve and exposes the accepted link's Remote,
// which pulls from and pushes to the remote side. The callback should retain
// the handle and return; requests are served after it returns. The handle
// becomes unavailable when this connection ends.
func ServeRemote(ctx context.Context, conn io.ReadWriteCloser, offer Offer, blobs Blobs, accepted func(Accepted, *Remote)) error {
	sv := &server{conn: conn, blobs: blobs, sem: make(chan struct{}, 1), searches: map[uint32]context.CancelFunc{}, scanning: make(chan struct{}, searchers)}
	sv.searchCtx, sv.searchStop = context.WithCancel(ctx)
	defer sv.searchStop()
	sv.remote = &Remote{sv: sv, pending: map[uint32]*pending{}, done: make(chan struct{})}
	sv.queue.cond.L = &sv.queue.mu
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		sv.mu.Lock()
		sv.stopping = true
		bye := sv.offered
		sv.mu.Unlock()
		sv.remote.end(ctx.Err())
		if bye {
			sv.goodbye()
		}
		conn.Close()
	})
	defer func() {
		if !stop() {
			<-stopped
		}
	}()
	err := sv.run(offer, func(a Accepted) {
		if accepted != nil {
			accepted(a, sv.remote)
		}
	})
	conn.Close()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	sv.remote.end(err)
	return err
}

// server is the source side of one link.
type server struct {
	conn   io.ReadWriteCloser
	blobs  Blobs
	remote *Remote
	// sem serializes frame writes; waiting writers are served in arrival order, which interleaves the streams.
	sem   chan struct{}
	queue queue
	// searchCtx ends every search request when the link ends; scanning bounds those scanned at once.
	searchCtx  context.Context
	searchStop context.CancelFunc
	scanning   chan struct{}
	searching  sync.WaitGroup

	mu       sync.Mutex
	offered  bool                          // the offer is out, so a goodbye cannot be mistaken for it
	stopping bool                          // ctx is done
	err      error                         // first failure of a worker
	searches map[uint32]context.CancelFunc // running search requests by tag
}

// run performs the handshake and then serves requests until the link ends.
func (sv *server) run(offer Offer, accepted func(Accepted)) error {
	payload, err := encodeOffer(offer)
	if err != nil {
		return err
	}
	if err := sv.send(frame(frameOffer, payload)); err != nil {
		return fmt.Errorf("send offer: %w", err)
	}
	sv.mu.Lock()
	sv.offered = !sv.stopping
	sv.mu.Unlock()
	fr := newFrameReader(sv.conn)
	_, p, err := fr.next(frameAnswer)
	if errors.Is(err, io.EOF) {
		return errors.New("remote closed the link before answering the offer")
	}
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	body, err := checkHello(p, "remote")
	if err != nil {
		return err
	}
	var ans answer
	if err := json.Unmarshal(body, &ans); err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	if ans.Error != "" {
		return fmt.Errorf("remote refused the link: %s", ans.Error)
	}
	if ans.Accepted == nil {
		return errors.New("read answer: neither accepted nor refused")
	}
	if accepted != nil {
		accepted(*ans.Accepted)
	}

	var wg sync.WaitGroup
	for range streams {
		wg.Go(sv.work)
	}
	err = sv.read(fr)
	sv.remote.end(err)
	sv.queue.close()
	sv.searchStop()
	sv.conn.Close()
	wg.Wait()
	sv.searching.Wait()
	sv.mu.Lock()
	defer sv.mu.Unlock()
	switch {
	case errors.Is(err, errBye):
		return nil
	case sv.err != nil:
		return sv.err
	case errors.Is(err, io.EOF):
		return errors.New("link lost: remote closed the stream without saying goodbye")
	}
	return fmt.Errorf("link lost: %w", err)
}

// read queues the remote's requests until it says goodbye or the stream fails.
func (sv *server) read(fr *frameReader) error {
	for {
		typ, p, err := fr.next(0)
		if err != nil {
			return err
		}
		switch typ {
		case frameRequest:
			reqs, err := decodeRequests(p)
			if err != nil {
				return fmt.Errorf("protocol error: %w", err)
			}
			sv.queue.push(reqs)
		case frameMeta, frameMetaEnd, frameBlob, frameChunk, framePulled, framePullErr, framePushing, framePushed, framePushErr:
			if err := sv.remote.receive(typ, p); err != nil {
				return err
			}
		case frameSearch:
			if err := sv.startSearch(p); err != nil {
				return err
			}
		case frameAbort:
			if err := sv.abortSearch(p); err != nil {
				return err
			}
		case frameBye:
			return errBye
		default:
			return fmt.Errorf("protocol error: unexpected frame %q", typ)
		}
	}
}

// work streams queued blobs one at a time until the queue closes; a write failure ends the link.
func (sv *server) work() {
	buf := make([]byte, headerSize+12+chunkSize)
	for {
		r, ok := sv.queue.pop()
		if !ok {
			return
		}
		if err := sv.stream(r, buf); err != nil {
			sv.mu.Lock()
			if sv.err == nil {
				sv.err = fmt.Errorf("link lost: %w", err)
			}
			sv.mu.Unlock()
			sv.conn.Close()
			return
		}
	}
}

// stream sends one blob as a begin frame and data frames (or an error frame
// when the blob cannot be produced). Only a failed write is returned.
func (sv *server) stream(r request, buf []byte) error {
	rc, size, err := sv.blobs.Open(r.id, r.path)
	if err != nil {
		return sv.fail(r.tag, err.Error())
	}
	defer rc.Close()
	if size < 0 {
		return sv.fail(r.tag, fmt.Sprintf("%s: negative size %d", r.path, size))
	}
	sent := int64(0)
	for first := true; first || sent < size; first = false {
		typ, off := frameData, headerSize+4
		if first {
			typ, off = frameBegin, headerSize+12
			binary.BigEndian.PutUint64(buf[headerSize+4:], uint64(size))
		}
		binary.BigEndian.PutUint32(buf[headerSize:], r.tag)
		n := int(min(size-sent, chunkSize))
		if _, err := io.ReadFull(rc, buf[off:off+n]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return sv.fail(r.tag, fmt.Sprintf("%s: content ended before its announced %d bytes", r.path, size))
			}
			return sv.fail(r.tag, fmt.Sprintf("read %s: %v", r.path, err))
		}
		f := buf[:off+n]
		seal(f, typ)
		if err := sv.send(f); err != nil {
			return err
		}
		sent += int64(n)
	}
	return nil
}

// fail tells the remote that the blob with tag cannot be produced.
func (sv *server) fail(tag uint32, msg string) error {
	return sv.send(frame(frameError, binary.BigEndian.AppendUint32(nil, tag), clip(msg)))
}

// send writes one frame, waiting its turn behind other writers.
func (sv *server) send(f []byte) error {
	sv.sem <- struct{}{}
	defer func() { <-sv.sem }()
	_, err := sv.conn.Write(f)
	return err
}

// goodbye tells the remote the link is closing on purpose, giving up after byeTimeout if the stream is stuck.
func (sv *server) goodbye() {
	sent := make(chan struct{})
	go func() {
		sv.send(frame(frameBye))
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(byeTimeout):
	}
}

// Remote asks the remote side of an accepted source-side link for work: Pull
// its checkout's changes, Push the content it still reads through the link.
type Remote struct {
	sv      *server
	mu      sync.Mutex
	next    uint32
	pending map[uint32]*pending
	done    chan struct{}
	err     error
}

// pending is one pull or push awaiting the remote's answer, touched under Remote.mu.
type pending struct {
	done     chan struct{}
	err      error
	push     bool         // answered by framePushing, framePushed or framePushErr; else a pull
	progress func(Pushed) // a push's progress callback, if any
	pushed   Pushed       // a push's final answer
	// A pull's answer, assembled as its frames arrive.
	result Pulled
	meta   []byte
	ready  bool
	blob   store.OID
	size   uint64
	data   []byte
}

// Pull snapshots the remote checkout and returns source changes since from.
// Cancellation abandons this request without interrupting concurrent fetches.
func (p *Remote) Pull(ctx context.Context, from store.OID) (Pulled, error) {
	raw, ok := hexID(from)
	if !ok {
		return Pulled{}, fmt.Errorf("pull: malformed base tree id %q", from)
	}
	c, err := p.ask(ctx, framePull, raw[:], nil)
	if err != nil {
		return Pulled{}, err
	}
	return c.result, nil
}

// Push has the remote fetch, over this link, every blob its checkouts attached
// from this machine still lack (see Session.SetPushHandler), so they stay
// readable once the link is gone. progress, unless nil, receives the remote's
// reports in order on the link's reader, so it must not block; it may still
// run briefly after a canceled Push returned. Cancellation stops the remote
// fetching more; blobs already requested still land there.
func (p *Remote) Push(ctx context.Context, progress func(Pushed)) (Pushed, error) {
	c, err := p.ask(ctx, framePush, nil, progress)
	if err != nil {
		return Pushed{}, err
	}
	return c.pushed, nil
}

// ask sends a pull or push request of typ under a fresh tag, followed by body,
// and waits for the remote's answer, the end of the link or ctx; a request
// abandoned by ctx is canceled on the remote too. progress is a push's.
func (p *Remote) ask(ctx context.Context, typ byte, body []byte, progress func(Pushed)) (*pending, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return nil, err
	}
	tag := p.next
	p.next++
	c := &pending{done: make(chan struct{}), push: typ == framePush, progress: progress}
	p.pending[tag] = c
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, tag)
		p.mu.Unlock()
	}()
	sent := make(chan error, 1)
	go func() { sent <- p.sv.send(frame(typ, binary.BigEndian.AppendUint32(nil, tag), body)) }()
	select {
	case err := <-sent:
		if err != nil {
			what := "pull"
			if c.push {
				what = "push"
			}
			return nil, fmt.Errorf("%s request: %w", what, err)
		}
	case <-ctx.Done():
		go func() {
			<-sent
			p.cancel(tag)
		}()
		return nil, ctx.Err()
	case <-p.done:
		p.mu.Lock()
		err := p.err
		p.mu.Unlock()
		return nil, err
	}
	select {
	case <-c.done:
		return c, c.err
	case <-ctx.Done():
		p.cancel(tag)
		return nil, ctx.Err()
	}
}

// cancel asks the remote to abandon a pull or push without blocking its caller.
func (p *Remote) cancel(tag uint32) {
	go p.sv.send(frame(frameCancel, binary.BigEndian.AppendUint32(nil, tag)))
}

// end releases every pending request when the serving connection ends.
func (p *Remote) end(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return
	}
	if err == nil || errors.Is(err, errBye) {
		err = errClosed
	}
	p.err = err
	close(p.done)
	for _, c := range p.pending {
		c.err = err
		close(c.done)
	}
	clear(p.pending)
}

// receive consumes a bounded pull or push response frame; canceled tags are discarded.
func (p *Remote) receive(typ byte, body []byte) error {
	if len(body) < 4 {
		return fmt.Errorf("protocol error: short response frame %q", typ)
	}
	tag := binary.BigEndian.Uint32(body)
	body = body[4:]
	isPush := typ == framePushing || typ == framePushed || typ == framePushErr
	if typ == framePushing {
		// Reported outside p.mu, so the callback may call into this Remote.
		pushed, err := decodePushed(body)
		if err != nil {
			return err
		}
		p.mu.Lock()
		c := p.pending[tag]
		p.mu.Unlock()
		if c != nil && !c.push {
			return fmt.Errorf("protocol error: frame %q answers request %d of another kind", typ, tag)
		}
		if c != nil && c.progress != nil {
			c.progress(pushed)
		}
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.pending[tag]
	if c == nil {
		return nil
	}
	if c.push != isPush {
		return fmt.Errorf("protocol error: frame %q answers request %d of another kind", typ, tag)
	}
	finish := func(err error) {
		c.err = err
		delete(p.pending, tag)
		close(c.done)
	}
	switch typ {
	case framePushErr:
		finish(fmt.Errorf("remote push: %s", body))
	case framePushed:
		pushed, err := decodePushed(body)
		if err != nil {
			return err
		}
		c.pushed = pushed
		finish(nil)
	case framePullErr:
		finish(fmt.Errorf("remote snapshot: %s", body))
	case frameMeta:
		if c.ready || len(body) > maxOfferJSON-len(c.meta) {
			return errors.New("protocol error: invalid pull metadata")
		}
		c.meta = append(c.meta, body...)
	case frameMetaEnd:
		if c.ready || len(body) != 0 {
			return errors.New("protocol error: repeated pull metadata")
		}
		if err := json.Unmarshal(c.meta, &c.result); err != nil {
			return fmt.Errorf("decode pull metadata: %w", err)
		}
		c.meta, c.ready = nil, true
		c.result.Data = map[store.OID][]byte{}
	case frameBlob:
		if !c.ready || c.blob != "" || len(body) != 28 {
			return errors.New("protocol error: invalid pull blob header")
		}
		c.blob = store.OID(fmt.Sprintf("%x", body[:20]))
		c.size = binary.BigEndian.Uint64(body[20:])
		if c.size > uint64(math.MaxInt) {
			return errors.New("protocol error: pull blob exceeds addressable memory")
		}
		c.data = nil
		if c.size == 0 {
			return c.finishBlob()
		}
		c.data = make([]byte, 0, int(c.size))
	case frameChunk:
		if c.blob == "" || uint64(len(body)) > c.size-uint64(len(c.data)) {
			return errors.New("protocol error: invalid pull blob chunk")
		}
		c.data = append(c.data, body...)
		if uint64(len(c.data)) == c.size {
			return c.finishBlob()
		}
	case framePulled:
		if !c.ready || c.blob != "" || len(body) != 0 {
			return errors.New("protocol error: incomplete pull response")
		}
		finish(nil)
	}
	return nil
}

// finishBlob verifies a complete pulled blob before exposing its bytes.
func (c *pending) finishBlob() error {
	if store.HashBytes("blob", c.data) != c.blob {
		return fmt.Errorf("pull blob %s: content hash mismatch", c.blob)
	}
	c.result.Data[c.blob] = c.data
	c.blob, c.data = "", nil
	return nil
}

// queue holds requests in arrival order for the streaming workers.
type queue struct {
	mu     sync.Mutex
	cond   sync.Cond
	items  []request
	closed bool
}

// push appends requests and wakes waiting workers.
func (q *queue) push(reqs []request) {
	q.mu.Lock()
	q.items = append(q.items, reqs...)
	q.mu.Unlock()
	q.cond.Broadcast()
}

// pop takes the oldest request, waiting for one; ok is false once the queue is closed.
func (q *queue) pop() (request, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return request{}, false
	}
	r := q.items[0]
	q.items = q.items[1:]
	if len(q.items) == 0 {
		q.items = nil
	}
	return r, true
}

// close drops pending requests and releases every waiting worker.
func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.mu.Unlock()
	q.cond.Broadcast()
}
