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
	return ServePull(ctx, conn, offer, blobs, func(a Accepted, _ *Puller) {
		if accepted != nil {
			accepted(a)
		}
	})
}

// ServePull serves blobs like Serve and exposes the active link's pull handle
// after acceptance. The callback should retain the handle and return; requests
// are served after it returns. The handle becomes unavailable when this connection ends.
func ServePull(ctx context.Context, conn io.ReadWriteCloser, offer Offer, blobs Blobs, accepted func(Accepted, *Puller)) error {
	sv := &server{conn: conn, blobs: blobs, sem: make(chan struct{}, 1), searches: map[uint32]context.CancelFunc{}, scanning: make(chan struct{}, searchers)}
	sv.searchCtx, sv.searchStop = context.WithCancel(ctx)
	defer sv.searchStop()
	sv.pull = &Puller{sv: sv, pending: map[uint32]*pullCall{}, done: make(chan struct{})}
	sv.queue.cond.L = &sv.queue.mu
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		sv.mu.Lock()
		sv.stopping = true
		bye := sv.offered
		sv.mu.Unlock()
		sv.pull.end(ctx.Err())
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
			accepted(a, sv.pull)
		}
	})
	conn.Close()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	sv.pull.end(err)
	return err
}

// server is the source side of one link.
type server struct {
	conn  io.ReadWriteCloser
	blobs Blobs
	pull  *Puller
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
	sv.pull.end(err)
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
		case frameMeta, frameMetaEnd, frameBlob, frameChunk, framePulled, framePullErr:
			if err := sv.pull.receive(typ, p); err != nil {
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

// Puller requests remote changes over an accepted source-side link.
type Puller struct {
	sv      *server
	mu      sync.Mutex
	next    uint32
	pending map[uint32]*pullCall
	done    chan struct{}
	err     error
}

// pullCall accumulates one pull, touched under Puller.mu.
type pullCall struct {
	done   chan struct{}
	result Pulled
	err    error
	meta   []byte
	ready  bool
	blob   store.OID
	size   uint64
	data   []byte
}

// Pull snapshots the remote checkout and returns source changes since from.
// Cancellation abandons this request without interrupting concurrent fetches.
func (p *Puller) Pull(ctx context.Context, from store.OID) (Pulled, error) {
	raw, ok := hexID(from)
	if !ok {
		return Pulled{}, fmt.Errorf("pull: malformed base tree id %q", from)
	}
	if err := ctx.Err(); err != nil {
		return Pulled{}, err
	}
	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return Pulled{}, err
	}
	tag := p.next
	p.next++
	c := &pullCall{done: make(chan struct{})}
	p.pending[tag] = c
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, tag)
		p.mu.Unlock()
	}()
	sent := make(chan error, 1)
	go func() { sent <- p.sv.send(frame(framePull, binary.BigEndian.AppendUint32(nil, tag), raw[:])) }()
	select {
	case err := <-sent:
		if err != nil {
			return Pulled{}, fmt.Errorf("pull request: %w", err)
		}
	case <-ctx.Done():
		go func() {
			<-sent
			p.cancel(tag)
		}()
		return Pulled{}, ctx.Err()
	case <-p.done:
		p.mu.Lock()
		err := p.err
		p.mu.Unlock()
		return Pulled{}, err
	}
	select {
	case <-c.done:
		return c.result, c.err
	case <-ctx.Done():
		p.cancel(tag)
		return Pulled{}, ctx.Err()
	}
}

// cancel asks the remote to abandon a pull without blocking its caller.
func (p *Puller) cancel(tag uint32) {
	go p.sv.send(frame(frameCancel, binary.BigEndian.AppendUint32(nil, tag)))
}

// end releases every pending pull when the serving connection ends.
func (p *Puller) end(err error) {
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

// receive consumes a bounded pull response frame; canceled tags are discarded.
func (p *Puller) receive(typ byte, body []byte) error {
	if len(body) < 4 {
		return fmt.Errorf("protocol error: short pull frame %q", typ)
	}
	tag := binary.BigEndian.Uint32(body)
	body = body[4:]
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.pending[tag]
	if c == nil {
		return nil
	}
	finish := func(err error) {
		c.err = err
		delete(p.pending, tag)
		close(c.done)
	}
	switch typ {
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
func (c *pullCall) finishBlob() error {
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
