package peer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/stencil-hq/ovnfs/internal/search/grep"
	"github.com/stencil-hq/ovnfs/store"
)

// searchCall collects one search request's results; touched under Session.mu.
type searchCall struct {
	done chan struct{}
	data []byte // results JSON so far
	err  error
	n    int // wants in the request
}

// settle completes c once; callers hold Session.mu.
func (c *searchCall) settle(err error) {
	select {
	case <-c.done:
	default:
		c.err = err
		close(c.done)
	}
}

// Search scans the wanted blobs with q on the source machine, which holds
// their bytes, and returns one outcome per want, in order. Only results cross
// the link: nothing enters the store. A blob the source cannot produce comes
// back unsearched. Fails when the link ends, ctx is done (the source is told
// to stop), or the source cannot run q.
func (s *Session) Search(ctx context.Context, q grep.Query, wants []Want) ([]grep.Outcome, error) {
	for _, w := range wants {
		if !w.ID.Valid() {
			return nil, fmt.Errorf("search %s: malformed id %q", w.Path, w.ID)
		}
		if len(w.Path) > maxPathHint {
			return nil, fmt.Errorf("search %s: path longer than %d bytes", w.ID, maxPathHint)
		}
	}
	query, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(query) > maxQuery {
		return nil, fmt.Errorf("search: query longer than %d bytes", maxQuery)
	}
	if len(wants) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	switch {
	case s.ended:
		err := s.failure
		s.mu.Unlock()
		return nil, fmt.Errorf("search: %w", err)
	case !s.replied:
		s.mu.Unlock()
		return nil, errors.New("search: link not accepted yet")
	}
	frames, tags := encodeSearch(&s.nextSearch, query, wants)
	calls := make([]*searchCall, len(frames))
	for i, tag := range tags {
		calls[i] = &searchCall{done: make(chan struct{}), n: tagWants(tags, i, len(wants))}
		s.searches[tag.tag] = calls[i]
	}
	s.mu.Unlock()
	s.post(frames...)

	outs := make([]grep.Outcome, 0, len(wants))
	for i, c := range calls {
		select {
		case <-c.done:
		case <-ctx.Done():
			s.abort(tags[i:])
			return nil, fmt.Errorf("search: %w", ctx.Err())
		}
		if c.err != nil {
			s.abort(tags[i+1:])
			return nil, fmt.Errorf("search: %w", c.err)
		}
		var part []grep.Outcome
		if err := json.Unmarshal(c.data, &part); err != nil {
			s.abort(tags[i+1:])
			return nil, fmt.Errorf("search: decode results: %w", err)
		}
		if len(part) != c.n {
			s.abort(tags[i+1:])
			return nil, fmt.Errorf("search: %d results for %d blobs", len(part), c.n)
		}
		outs = append(outs, part...)
	}
	return outs, nil
}

// abort forgets the given requests and tells the source to stop them.
func (s *Session) abort(tags []searchTag) {
	if len(tags) == 0 {
		return
	}
	frames := make([][]byte, 0, len(tags))
	s.mu.Lock()
	for _, t := range tags {
		if _, ok := s.searches[t.tag]; ok {
			delete(s.searches, t.tag)
			frames = append(frames, frame(frameAbort, binary.BigEndian.AppendUint32(nil, t.tag)))
		}
	}
	s.mu.Unlock()
	s.post(frames...)
}

// searchResponse takes one 'H' or 'Z' frame; frames for forgotten tags are dropped.
func (s *Session) searchResponse(typ byte, p []byte) error {
	if len(p) < 4 {
		return fmt.Errorf("protocol error: short search frame %q", typ)
	}
	tag := binary.BigEndian.Uint32(p)
	p = p[4:]
	s.mu.Lock()
	c := s.searches[tag]
	switch {
	case c == nil:
	case typ == frameFound:
		delete(s.searches, tag)
		if len(p) > 0 {
			c.settle(fmt.Errorf("source: %s", p))
		} else {
			c.settle(nil)
		}
	case len(c.data)+len(p) > maxResults:
		delete(s.searches, tag)
		c.data = nil
		c.settle(fmt.Errorf("results exceed %d bytes", maxResults))
		s.mu.Unlock()
		s.post(frame(frameAbort, binary.BigEndian.AppendUint32(nil, tag)))
		return nil
	default:
		c.data = append(c.data, p...)
	}
	s.mu.Unlock()
	return nil
}

// searchTag is one request frame of a search: its tag and the wants it carries, wants[lo:].
type searchTag struct {
	tag uint32
	lo  int
}

// tagWants is how many wants request i of a search over total wants carries.
func tagWants(tags []searchTag, i, total int) int {
	if i+1 < len(tags) {
		return tags[i+1].lo - tags[i].lo
	}
	return total - tags[i].lo
}

// encodeSearch packs a search into as many request frames as its size needs,
// taking their tags from *next.
func encodeSearch(next *uint32, query []byte, wants []Want) ([][]byte, []searchTag) {
	var frames [][]byte
	var tags []searchTag
	var b []byte
	open := func(lo int) {
		tags = append(tags, searchTag{tag: *next, lo: lo})
		b = binary.BigEndian.AppendUint32(make([]byte, headerSize, 4096), *next)
		*next++
		b = binary.AppendUvarint(b, uint64(len(query)))
		b = append(b, query...)
	}
	for i, w := range wants {
		if b == nil {
			open(i)
		}
		raw, _ := hexID(w.ID)
		b = append(b, raw[:]...)
		b = binary.AppendUvarint(b, uint64(len(w.Path)))
		b = append(b, w.Path...)
		if len(b) >= requestBatch+len(query) {
			seal(b, frameSearch)
			frames, b = append(frames, b), nil
		}
	}
	if b != nil {
		seal(b, frameSearch)
		frames = append(frames, b)
	}
	return frames, tags
}

// decodeSearch unpacks a search frame's payload.
func decodeSearch(p []byte) (uint32, grep.Query, []request, error) {
	var q grep.Query
	if len(p) < 4 {
		return 0, q, nil, errors.New("truncated search")
	}
	tag := binary.BigEndian.Uint32(p)
	p = p[4:]
	n, k := binary.Uvarint(p)
	if k <= 0 || n > uint64(len(p)-k) || n > maxQuery {
		return tag, q, nil, errors.New("truncated search query")
	}
	if err := json.Unmarshal(p[k:k+int(n)], &q); err != nil {
		return tag, q, nil, fmt.Errorf("search query: %w", err)
	}
	p = p[k+int(n):]
	var reqs []request
	for len(p) > 0 {
		if len(p) < 20 {
			return tag, q, nil, errors.New("truncated search id")
		}
		r := request{tag: tag, id: store.OID(fmt.Sprintf("%x", p[:20]))}
		p = p[20:]
		n, k := binary.Uvarint(p)
		if k <= 0 || n > uint64(len(p)-k) {
			return tag, q, nil, errors.New("truncated search path")
		}
		r.path = string(p[k : k+int(n)])
		p = p[k+int(n):]
		reqs = append(reqs, r)
	}
	return tag, q, reqs, nil
}

// startSearch runs one search request in the background, searchers at a time.
func (sv *server) startSearch(p []byte) error {
	tag, q, reqs, err := decodeSearch(p)
	if err != nil {
		return fmt.Errorf("protocol error: %w", err)
	}
	ctx, cancel := context.WithCancel(sv.searchCtx)
	sv.mu.Lock()
	if old := sv.searches[tag]; old != nil {
		old()
	}
	sv.searches[tag] = cancel
	sv.mu.Unlock()
	sv.searching.Go(func() {
		defer func() {
			sv.mu.Lock()
			delete(sv.searches, tag)
			sv.mu.Unlock()
			cancel()
		}()
		select {
		case sv.scanning <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-sv.scanning }()
		if err := sv.search(ctx, tag, q, reqs); err != nil {
			sv.mu.Lock()
			if sv.err == nil {
				sv.err = fmt.Errorf("link lost: %w", err)
			}
			sv.mu.Unlock()
			sv.conn.Close()
		}
	})
	return nil
}

// abortSearch stops a search request the remote no longer waits for.
func (sv *server) abortSearch(p []byte) error {
	if len(p) != 4 {
		return errors.New("protocol error: invalid search cancel")
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if cancel := sv.searches[binary.BigEndian.Uint32(p)]; cancel != nil {
		cancel()
	}
	return nil
}

// search scans the requested blobs and sends their outcomes, or nothing once
// ctx is done. Only a failed write is returned.
func (sv *server) search(ctx context.Context, tag uint32, q grep.Query, reqs []request) error {
	tagged := binary.BigEndian.AppendUint32(nil, tag)
	m, err := grep.NewMatcher(q)
	if err != nil {
		return sv.send(frame(frameFound, tagged, clip(err.Error())))
	}
	outs := make([]grep.Outcome, len(reqs))
	var buf []byte
	for i, r := range reqs {
		if ctx.Err() != nil {
			return nil
		}
		data, ok := sv.head(r, m.Limit(), buf)
		if !ok {
			continue
		}
		buf = data
		outs[i] = m.Scan(ctx, data)
	}
	if ctx.Err() != nil {
		return nil
	}
	body, err := json.Marshal(outs)
	if err != nil {
		return sv.send(frame(frameFound, tagged, clip(err.Error())))
	}
	for len(body) > 0 {
		if ctx.Err() != nil {
			return nil
		}
		n := min(len(body), chunkSize)
		if err := sv.send(frame(frameHits, tagged, body[:n])); err != nil {
			return err
		}
		body = body[n:]
	}
	return sv.send(frame(frameFound, tagged))
}

// head reads up to limit leading bytes of the requested blob into buf.
func (sv *server) head(r request, limit int64, buf []byte) ([]byte, bool) {
	rc, size, err := sv.blobs.Open(r.id, r.path)
	if err != nil {
		return buf, false
	}
	defer rc.Close()
	if size < 0 {
		return buf, false
	}
	n := int(min(size, limit))
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(rc, buf); err != nil {
		return buf, false
	}
	return buf, true
}
