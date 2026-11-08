package peer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/store"
)

// Want is a blob a remote reader waits for.
type Want struct {
	ID   store.OID
	Path string // leaf path the blob was offered under, passed to the source as a hint
	Size int64  // expected size; informational, the source's announced size wins
}

// Session is the remote side of one link.
type Session struct {
	conn io.ReadWriteCloser
	st   *store.Store
	fr   *frameReader
	done chan struct{}

	mu          sync.Mutex
	replied     bool
	leaving     bool // Close was called: whatever ends the link now is a clean close
	ended       bool
	reason      error                // why the link ended; nil for a clean close
	failure     error                // what waiters see once the link ended
	inflight    map[store.OID]*call  // requested blobs by id, until settled
	transfers   map[uint32]*transfer // requested blobs by tag, until their stream ends
	nextTag     uint32
	searches    map[uint32]*searchCall // search requests by tag, until their results end
	nextSearch  uint32
	pullHandler func(context.Context, store.OID) (Pulled, error)
	pull        *remotePull
	pushHandler func() []Want
	pushes      map[uint32]context.CancelFunc // running pushes by tag

	// outbox feeds the writer goroutine so Fetch never blocks on the stream.
	outMu      sync.Mutex
	out        []outFrame
	outClosed  bool
	wake       chan struct{}
	writerDone chan struct{}
}

// outFrame optionally acknowledges one frame after it reaches the stream.
type outFrame struct {
	data []byte
	done chan struct{}
}

// remotePull is the one remote snapshot currently being served.
type remotePull struct {
	tag    uint32
	ctx    context.Context
	cancel context.CancelFunc
}

// call is one in-flight blob that any number of Fetch calls wait for.
type call struct {
	done    chan struct{}
	err     error
	settled bool
}

// transfer is the receiving state of one tagged blob stream; only the read loop touches w, size and n.
type transfer struct {
	tag   uint32
	id    store.OID
	c     *call
	begun bool
	w     *store.BlobWriter // nil once the blob failed locally; its remaining bytes are discarded
	size  int64
	n     int64
}

// settled stands for a blob that turned out to be present already.
var settled = func() *call {
	c := &call{done: make(chan struct{}), settled: true}
	close(c.done)
	return c
}()

// Accept reads the offer from a new link stream; blobs fetched later land in st.
// An offer of another protocol version or a malformed offer is refused on the
// stream and returned as an error; conn is closed on any error.
func Accept(conn io.ReadWriteCloser, st *store.Store) (*Session, Offer, error) {
	fr := newFrameReader(conn)
	_, p, err := fr.next(frameOffer)
	if err != nil {
		conn.Close()
		return nil, Offer{}, fmt.Errorf("read offer: %w", err)
	}
	body, err := checkHello(p, "source")
	if err == nil {
		var o Offer
		if o, err = decodeOffer(body); err == nil {
			s := &Session{
				conn:       conn,
				st:         st,
				fr:         fr,
				done:       make(chan struct{}),
				inflight:   map[store.OID]*call{},
				transfers:  map[uint32]*transfer{},
				searches:   map[uint32]*searchCall{},
				pushes:     map[uint32]context.CancelFunc{},
				wake:       make(chan struct{}, 1),
				writerDone: make(chan struct{}),
			}
			return s, o, nil
		}
	}
	writeAnswer(conn, answer{Error: err.Error()})
	conn.Close()
	return nil, Offer{}, err
}

// writeAnswer sends the remote's handshake frame.
func writeAnswer(w io.Writer, a answer) error {
	if len(a.Error) > maxMessage {
		a.Error = string(clip(a.Error))
	}
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = w.Write(frame(frameAnswer, hello(), body))
	return err
}

// SetPullHandler configures how source-initiated pulls snapshot this remote checkout.
// It must be called before Reply; a nil handler refuses pull requests.
func (s *Session) SetPullHandler(fn func(context.Context, store.OID) (Pulled, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pullHandler = fn
}

// SetPushHandler configures what source-initiated pushes fetch: every blob this
// machine still reads through the link, which fn lists. It must be called
// before Reply; a nil handler refuses pushes.
func (s *Session) SetPushHandler(fn func() []Want) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushHandler = fn
}

// Reply sends the acceptance and starts serving fetches; Refuse sends err's message and closes.
func (s *Session) Reply(a Accepted) error {
	s.mu.Lock()
	if s.replied || s.ended {
		s.mu.Unlock()
		return errors.New("reply: link already answered or closed")
	}
	s.mu.Unlock()
	if err := writeAnswer(s.conn, answer{Accepted: &a}); err != nil {
		err = fmt.Errorf("reply: %w", err)
		s.end(err)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return fmt.Errorf("reply: %w", s.failure)
	}
	s.replied = true
	go s.read()
	go s.write()
	return nil
}

// Refuse answers the offer with err's message and ends the link cleanly.
func (s *Session) Refuse(err error) error {
	s.mu.Lock()
	if s.replied || s.ended {
		s.mu.Unlock()
		return errors.New("refuse: link already answered or closed")
	}
	s.mu.Unlock()
	werr := writeAnswer(s.conn, answer{Error: err.Error()})
	s.end(nil)
	if werr != nil {
		return fmt.Errorf("refuse: %w", werr)
	}
	return nil
}

// Fetch makes every wanted blob present in st. Ids already in st are skipped, ids already in flight (from any caller) are waited for, not requested twice. Fails when the link ends, ctx is done, or the source cannot produce a blob (the source's error message is kept). A blob whose bytes do not hash to its id is an error, never stored.
// A Fetch abandoned by ctx leaves its blobs in flight; they still land in st.
func (s *Session) Fetch(ctx context.Context, wants []Want) error {
	return s.fetch(ctx, wants, nil)
}

// fetch is Fetch calling landed, unless nil, once for each want as its blob is
// present: first those already stored, then the rest in want order.
func (s *Session) fetch(ctx context.Context, wants []Want, landed func(Want)) error {
	for _, w := range wants {
		if !w.ID.Valid() {
			return fmt.Errorf("fetch %s: malformed id %q", w.Path, w.ID)
		}
		if len(w.Path) > maxPathHint {
			return fmt.Errorf("fetch %s: path longer than %d bytes", w.ID, maxPathHint)
		}
	}
	missing := make([]Want, 0, len(wants))
	for _, w := range wants {
		switch {
		case !s.st.Has(w.ID):
			missing = append(missing, w)
		case landed != nil:
			landed(w)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	calls := make([]*call, len(missing))
	var reqs []request
	s.mu.Lock()
	switch {
	case s.ended:
		err := s.failure
		s.mu.Unlock()
		return fmt.Errorf("fetch: %w", err)
	case !s.replied:
		s.mu.Unlock()
		return errors.New("fetch: link not accepted yet")
	}
	for i, w := range missing {
		c := s.inflight[w.ID]
		// A blob settles only after it is published, so one neither in
		// flight nor present now was not finished by a racing transfer.
		if c == nil && s.st.Has(w.ID) {
			calls[i] = settled
			continue
		}
		if c == nil {
			c = &call{done: make(chan struct{})}
			tag := s.nextTag
			s.nextTag++
			s.inflight[w.ID] = c
			s.transfers[tag] = &transfer{tag: tag, id: w.ID, c: c}
			reqs = append(reqs, request{tag: tag, id: w.ID, path: w.Path})
		}
		calls[i] = c
	}
	s.mu.Unlock()
	if len(reqs) > 0 {
		s.post(encodeRequests(reqs)...)
	}
	for i, c := range calls {
		select {
		case <-c.done:
			if c.err != nil {
				return fmt.Errorf("fetch %s (%s): %w", missing[i].Path, missing[i].ID, c.err)
			}
			if landed != nil {
				landed(missing[i])
			}
		case <-ctx.Done():
			return fmt.Errorf("fetch %s (%s): %w", missing[i].Path, missing[i].ID, ctx.Err())
		}
	}
	return nil
}

// Done is closed when the link ends; Err then says why (nil for a clean close).
func (s *Session) Done() <-chan struct{} { return s.done }

// Err says why the link ended: nil while it is up or after a clean close by either side.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// Close says goodbye to the source (waiting at most byeTimeout for the stream), ends the link and fails pending fetches. It always returns nil.
func (s *Session) Close() error {
	s.mu.Lock()
	open := s.replied && !s.ended
	s.leaving = true
	s.mu.Unlock()
	if open {
		s.outMu.Lock()
		if !s.outClosed {
			s.out = append(s.out, outFrame{data: frame(frameBye)})
			s.outClosed = true
		}
		s.outMu.Unlock()
		s.poke()
		select {
		case <-s.writerDone:
		case <-time.After(byeTimeout):
		}
	}
	s.end(nil)
	return nil
}

// end records why the link ended (first reason wins), fails every waiter and closes the stream.
func (s *Session) end(reason error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	if s.leaving {
		reason = nil
	}
	s.reason = reason
	s.failure = errClosed
	if reason != nil {
		s.failure = fmt.Errorf("link lost: %w", reason)
	}
	for _, c := range s.inflight {
		c.settle(s.failure)
	}
	clear(s.inflight)
	for _, c := range s.searches {
		c.settle(s.failure)
	}
	clear(s.searches)
	if s.pull != nil {
		s.pull.cancel()
	}
	for _, cancel := range s.pushes {
		cancel()
	}
	close(s.done)
	s.mu.Unlock()
	s.conn.Close()
}

// settle completes a call once; callers hold Session.mu.
func (c *call) settle(err error) {
	if c.settled {
		return
	}
	c.settled, c.err = true, err
	close(c.done)
}

// post queues frames for the writer goroutine; frames posted after goodbye are dropped.
func (s *Session) post(frames ...[]byte) {
	s.outMu.Lock()
	if !s.outClosed {
		for _, f := range frames {
			s.out = append(s.out, outFrame{data: f})
		}
	}
	s.outMu.Unlock()
	s.poke()
}

// poke wakes the writer goroutine.
func (s *Session) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// write drains the outbox onto the stream until goodbye is sent or the link ends.
func (s *Session) write() {
	defer close(s.writerDone)
	for {
		s.outMu.Lock()
		q, closed := s.out, s.outClosed
		s.out = nil
		s.outMu.Unlock()
		if len(q) > 0 {
			for _, f := range q {
				bufs := net.Buffers{f.data}
				if _, err := bufs.WriteTo(s.conn); err != nil {
					s.end(fmt.Errorf("send: %w", err))
					return
				}
				if f.done != nil {
					close(f.done)
				}
			}
			continue
		}
		if closed {
			return
		}
		select {
		case <-s.wake:
		case <-s.done:
			return
		}
	}
}

// read receives blob streams until the link ends, then discards unfinished blobs.
func (s *Session) read() {
	err := s.receive()
	if errors.Is(err, errBye) {
		err = nil
	} else if errors.Is(err, io.EOF) {
		err = errors.New("source closed the stream without saying goodbye")
	}
	s.end(err)
	s.mu.Lock()
	ts := s.transfers
	s.transfers = nil
	s.mu.Unlock()
	for _, t := range ts {
		if t.w != nil {
			t.w.Abort()
		}
	}
}

// receive handles frames from the source; it returns errBye on goodbye.
func (s *Session) receive() error {
	for {
		typ, p, err := s.fr.next(0)
		if err != nil {
			return err
		}
		if typ == frameBye {
			return errBye
		}
		if typ == framePull || typ == framePush || typ == frameCancel {
			if err := s.request(typ, p); err != nil {
				return err
			}
			continue
		}
		if typ == frameHits || typ == frameFound {
			if err := s.searchResponse(typ, p); err != nil {
				return err
			}
			continue
		}
		if len(p) < 4 {
			return fmt.Errorf("protocol error: frame %q too short", typ)
		}
		t := s.transfer(binary.BigEndian.Uint32(p))
		if t == nil {
			return fmt.Errorf("protocol error: frame %q for unknown stream %d", typ, binary.BigEndian.Uint32(p))
		}
		switch typ {
		case frameBegin:
			if t.begun || len(p) < 12 {
				return fmt.Errorf("protocol error: bad begin for stream %d", t.tag)
			}
			size := binary.BigEndian.Uint64(p[4:])
			if size > math.MaxInt64 {
				return fmt.Errorf("protocol error: stream %d announces %d bytes", t.tag, size)
			}
			t.begun, t.size = true, int64(size)
			if t.w, err = s.st.NewBlobWriter(t.size); err != nil {
				s.settle(t, fmt.Errorf("stage blob: %w", err))
			}
			if err := s.absorb(t, p[12:]); err != nil {
				return err
			}
		case frameData:
			if !t.begun {
				return fmt.Errorf("protocol error: data before begin on stream %d", t.tag)
			}
			if err := s.absorb(t, p[4:]); err != nil {
				return err
			}
		case frameError:
			if t.w != nil {
				t.w.Abort()
				t.w = nil
			}
			s.settle(t, fmt.Errorf("source: %s", p[4:]))
			s.drop(t)
		default:
			return fmt.Errorf("protocol error: unexpected frame %q", typ)
		}
	}
}

// request starts or cancels a pull or push without blocking the fetch reader.
func (s *Session) request(typ byte, body []byte) error {
	size := 4
	if typ == framePull {
		size += 20
	}
	if len(body) != size {
		return fmt.Errorf("protocol error: invalid %q request", typ)
	}
	tag := binary.BigEndian.Uint32(body)
	switch typ {
	case framePull:
		s.startPull(tag, store.OID(fmt.Sprintf("%x", body[4:])))
	case framePush:
		return s.startPush(tag)
	case frameCancel:
		s.mu.Lock()
		if s.pull != nil && s.pull.tag == tag {
			s.pull.cancel()
		}
		if cancel := s.pushes[tag]; cancel != nil {
			cancel()
		}
		s.mu.Unlock()
	}
	return nil
}

// startPull snapshots the checkout in the background and streams its changes since from.
func (s *Session) startPull(tag uint32, from store.OID) {
	s.mu.Lock()
	fn := s.pullHandler
	if s.pull != nil || fn == nil || s.ended {
		msg := "remote checkout does not support snapshots"
		if s.pull != nil {
			msg = "another remote snapshot is in progress"
		}
		s.mu.Unlock()
		s.post(frame(framePullErr, binary.BigEndian.AppendUint32(nil, tag), clip(msg)))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &remotePull{tag: tag, ctx: ctx, cancel: cancel}
	s.pull = r
	s.mu.Unlock()
	go func() {
		defer cancel()
		p, err := fn(ctx, from)
		if err == nil {
			err = s.streamPull(r, p)
		}
		s.mu.Lock()
		if s.pull == r {
			s.pull = nil
		}
		s.mu.Unlock()
		if ctx.Err() == nil {
			if err != nil {
				s.sendPull(r, framePullErr, clip(err.Error()))
			} else {
				s.sendPull(r, framePulled)
			}
		}
	}()
}

// Push batch limits: a push fetches at most this much at a time, so a read on
// this machine waits behind one batch at the source, not the whole push.
// Progress goes to the source at most every pushReport.
const (
	pushBatchBlobs = 1024
	pushBatchBytes = 8 << 20
	pushReport     = 250 * time.Millisecond
)

// startPush fetches, in the background, every blob the push handler lists and
// answers with their count and size, or the first failure. Reusing the tag of
// a running push is a protocol error.
func (s *Session) startPush(tag uint32) error {
	answer := func(typ byte, parts ...[]byte) {
		s.post(frame(typ, append([][]byte{binary.BigEndian.AppendUint32(nil, tag)}, parts...)...))
	}
	s.mu.Lock()
	fn := s.pushHandler
	if s.pushes[tag] != nil {
		s.mu.Unlock()
		return fmt.Errorf("protocol error: push %d is already running", tag)
	}
	if fn == nil {
		s.mu.Unlock()
		answer(framePushErr, []byte("remote checkout does not take pushes"))
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.pushes[tag] = cancel
	s.mu.Unlock()
	go func() {
		defer cancel()
		pushed, err := s.push(ctx, fn(), func(p Pushed) { answer(framePushing, appendPushed(nil, p)) })
		s.mu.Lock()
		delete(s.pushes, tag)
		s.mu.Unlock()
		switch {
		case ctx.Err() != nil: // canceled by the source, or the link ended
		case err != nil:
			answer(framePushErr, clip(err.Error()))
		default:
			answer(framePushed, appendPushed(nil, pushed))
		}
	}()
	return nil
}

// push fetches wants in batches of at most pushBatchBlobs blobs and
// pushBatchBytes bytes (a larger blob alone), stopping at the first failure.
// It reports the totals before fetching, then progress at most every
// pushReport as blobs land.
func (s *Session) push(ctx context.Context, wants []Want, report func(Pushed)) (Pushed, error) {
	var p Pushed
	for _, w := range wants {
		p.TotalBlobs++
		p.TotalBytes += w.Size
	}
	report(p)
	last := time.Now()
	landed := func(w Want) {
		p.Blobs++
		p.Bytes += w.Size
		if time.Since(last) >= pushReport {
			report(p)
			last = time.Now()
		}
	}
	for len(wants) > 0 {
		n, size := 1, wants[0].Size
		for n < len(wants) && n < pushBatchBlobs && size+wants[n].Size <= pushBatchBytes {
			size += wants[n].Size
			n++
		}
		if err := s.fetch(ctx, wants[:n], landed); err != nil {
			return p, err
		}
		wants = wants[n:]
	}
	return p, nil
}

// sendPull queues one bounded response frame and waits for the writer to drain
// it, so a slow source never builds an unbounded outbox of blob chunks.
func (s *Session) sendPull(r *remotePull, typ byte, parts ...[]byte) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	body := [][]byte{binary.BigEndian.AppendUint32(nil, r.tag)}
	body = append(body, parts...)
	f := outFrame{data: frame(typ, body...), done: make(chan struct{})}
	s.outMu.Lock()
	if s.outClosed {
		s.outMu.Unlock()
		return errClosed
	}
	s.out = append(s.out, f)
	s.outMu.Unlock()
	s.poke()
	select {
	case <-f.done:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-s.done:
		return errClosed
	}
}

// streamPull sends metadata separately from raw blobs, with no blob-size frame limit.
func (s *Session) streamPull(r *remotePull, p Pulled) error {
	meta, err := json.Marshal(Pulled{Snapshot: p.Snapshot, Base: p.Base, Files: p.Files})
	if err != nil {
		return fmt.Errorf("encode pull snapshot: %w", err)
	}
	if len(meta) > maxOfferJSON {
		return errors.New("pull snapshot metadata exceeds protocol limit")
	}
	for len(meta) > 0 {
		n := min(len(meta), chunkSize)
		if err := s.sendPull(r, frameMeta, meta[:n]); err != nil {
			return err
		}
		meta = meta[n:]
	}
	if err := s.sendPull(r, frameMetaEnd); err != nil {
		return err
	}
	for id, data := range p.Data {
		raw, ok := hexID(id)
		if !ok {
			return fmt.Errorf("pull snapshot has invalid blob id %q", id)
		}
		header := binary.BigEndian.AppendUint64(raw[:], uint64(len(data)))
		if err := s.sendPull(r, frameBlob, header); err != nil {
			return err
		}
		for len(data) > 0 {
			n := min(len(data), chunkSize)
			if err := s.sendPull(r, frameChunk, data[:n]); err != nil {
				return err
			}
			data = data[n:]
		}
	}
	return nil
}

// absorb takes the next chunk of t, publishing the blob once all of it arrived.
func (s *Session) absorb(t *transfer, data []byte) error {
	if int64(len(data)) > t.size-t.n {
		return fmt.Errorf("protocol error: stream %d overruns its %d bytes", t.tag, t.size)
	}
	t.n += int64(len(data))
	if t.w != nil {
		if _, err := t.w.Write(data); err != nil {
			t.w.Abort()
			t.w = nil
			s.settle(t, fmt.Errorf("stage blob: %w", err))
		}
	}
	if t.n < t.size {
		return nil
	}
	if t.w != nil {
		s.settle(t, t.w.Commit(t.id))
		t.w = nil
	}
	s.drop(t)
	return nil
}

// transfer looks up the stream with tag.
func (s *Session) transfer(tag uint32) *transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transfers[tag]
}

// settle completes t's waiters with err and stops treating its id as in flight.
func (s *Session) settle(t *transfer, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[t.id] == t.c {
		delete(s.inflight, t.id)
	}
	t.c.settle(err)
}

// drop forgets a stream whose bytes have all arrived or that the source abandoned.
func (s *Session) drop(t *transfer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.transfers, t.tag)
}
