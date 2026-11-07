package store

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Chunk objects use a separate hash domain, so their bodies cannot be confused
// with Git trees or blobs. Manifests form a bounded fanout tree; only its root
// uses the Git blob id. Every reference records its reconstructed byte length.
const (
	chunkMin       = 8 << 10
	chunkMax       = 32 << 10
	manifestFanout = 128
	manifestDepth  = 16
	manifestHeader = 8
	manifestRecord = 28
	// gearWindow is how many trailing bytes the gear hash depends on.
	gearWindow = 64
	// gearShift keeps the gear hash's top 13 bits, which must be zero at a
	// boundary: past chunkMin, one follows every 8 KiB on average.
	gearShift = 64 - 13
)

type chunkRef struct {
	id   rawID
	size int64
}

func manifestData(refs []chunkRef) []byte {
	data := make([]byte, manifestHeader+manifestRecord*len(refs))
	var total uint64
	for i, ref := range refs {
		off := manifestHeader + i*manifestRecord
		copy(data[off:], ref.id[:])
		binary.LittleEndian.PutUint64(data[off+20:], uint64(ref.size))
		total += uint64(ref.size)
	}
	binary.LittleEndian.PutUint64(data, total)
	return data
}

func parseManifest(data []byte) ([]chunkRef, int64, error) {
	if len(data) < manifestHeader || (len(data)-manifestHeader)%manifestRecord != 0 || len(data) > manifestHeader+manifestFanout*manifestRecord {
		return nil, 0, errCorrupt
	}
	refs := make([]chunkRef, (len(data)-manifestHeader)/manifestRecord)
	var total uint64
	for i := range refs {
		off := manifestHeader + i*manifestRecord
		copy(refs[i].id[:], data[off:])
		n := binary.LittleEndian.Uint64(data[off+20:])
		if n == 0 || n > 1<<60 || total > 1<<60-n {
			return nil, 0, errCorrupt
		}
		total += n
		refs[i].size = int64(n)
	}
	if total != binary.LittleEndian.Uint64(data) {
		return nil, 0, errCorrupt
	}
	return refs, int64(total), nil
}

func manifestFrame(id rawID, data []byte, flags uint8) []byte {
	frame := make([]byte, frameHeader, frameHeader+len(data))
	frame = append(frame, data...)
	sealFrame(frame, id, flags|frameManifest, 0, time.Now().UnixNano())
	return frame
}

// chunkBuilder retains at most one page at each level while ingesting bytes.
// A boundary is placed where the gear hash's top bits are zero; bit k of the
// hash depends on the last k+1 bytes, so the top bits depend on the whole
// 64-byte window and nothing before it, allowing boundaries to resynchronize
// after insertions. Chunks span 8 to 32 KiB, roughly 16 KiB on average.
// Pages group 128 references by position: an insertion may regroup subsequent
// pages, but already-stored file chunks remain deduplicated.
type chunkBuilder struct {
	s      *Store
	levels [manifestDepth][]chunkRef
	// buf holds the bytes of the current chunk carried over between writes.
	buf []byte
	// gear hashes buf from chunkMin-gearWindow on, the only bytes any cut
	// test reads; it is zero before that.
	gear uint64
}

var gearTable = func() [256]uint64 {
	var table [256]uint64
	x := uint64(0x9e3779b97f4a7c15)
	for i := range table {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
		z = (z ^ z>>27) * 0x94d049bb133111eb
		table[i] = z ^ z>>31
	}
	return table
}()

func (b *chunkBuilder) add(ref chunkRef, level int) error {
	if level >= len(b.levels) {
		return fmt.Errorf("blob exceeds manifest depth")
	}
	b.levels[level] = append(b.levels[level], ref)
	if len(b.levels[level]) < manifestFanout {
		return nil
	}
	refs := b.levels[level]
	data := manifestData(refs)
	id := rawOf(HashBytes("manifest", data))
	if _, err := b.s.packs.add(manifestFrame(id, data, frameInternal)); err != nil {
		return err
	}
	b.levels[level] = refs[:0]
	return b.add(chunkRef{id, int64(binary.LittleEndian.Uint64(data))}, level+1)
}

// flush stores chunk, which is buf or a whole chunk of the caller's input,
// compressing it only when it is not stored yet, and starts the next chunk.
func (b *chunkBuilder) flush(chunk []byte) error {
	b.buf, b.gear = b.buf[:0], 0
	if len(chunk) == 0 {
		return nil
	}
	id := rawOf(HashBytes("chunk", chunk))
	now := time.Now().UnixNano()
	if !b.s.packs.touch(id, now) {
		if _, err := b.s.packs.add(encodeFrame(id, chunk, now, frameChunk)); err != nil {
			return err
		}
	}
	return b.add(chunkRef{id, int64(len(chunk))}, 0)
}

// write cuts data at content-defined boundaries, carrying a trailing partial
// chunk over to the next call; the boundaries do not depend on how the input
// is split between calls.
func (b *chunkBuilder) write(data []byte) error {
	for len(data) > 0 {
		n := len(b.buf)
		end := min(len(data), chunkMax-n)
		// Bytes before chunkMin-gearWindow cannot reach the hash of any
		// position a cut is tested at, so hashing starts there.
		i := min(max(chunkMin-gearWindow-n, 0), end)
		g := b.gear
		// Cuts are tested from the byte that makes the chunk chunkMin long.
		for warm := min(end, chunkMin-1-n); i < warm; i++ {
			g = g<<1 + gearTable[data[i]]
		}
		cut := n+end == chunkMax
		for ; i < end; i++ {
			g = g<<1 + gearTable[data[i]]
			if g>>gearShift == 0 {
				i, cut = i+1, true
				break
			}
		}
		chunk := data[:i]
		data = data[i:]
		if !cut {
			b.buf, b.gear = append(b.buf, chunk...), g
			continue
		}
		if n > 0 {
			chunk = append(b.buf, chunk...)
		}
		if err := b.flush(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (b *chunkBuilder) finish(id OID) error {
	b.s.mu.RLock()
	defer b.s.mu.RUnlock()
	b.s.publishMu.Lock()
	defer b.s.publishMu.Unlock()
	if b.s.refresh(id, b.s.objectPath(id), nil, false) == nil {
		return nil
	}
	if err := b.flush(b.buf); err != nil {
		return err
	}
	for level := range b.levels {
		refs := b.levels[level]
		if len(refs) == 0 {
			continue
		}
		higher := false
		for _, next := range b.levels[level+1:] {
			higher = higher || len(next) > 0
		}
		if !higher {
			_, err := b.s.packs.add(manifestFrame(rawOf(id), manifestData(refs), 0))
			return err
		}
		data := manifestData(refs)
		node := rawOf(HashBytes("manifest", data))
		if _, err := b.s.packs.add(manifestFrame(node, data, frameInternal)); err != nil {
			return err
		}
		b.levels[level] = nil
		if err := b.add(chunkRef{node, int64(binary.LittleEndian.Uint64(data))}, level+1); err != nil {
			return err
		}
	}
	return errCorrupt
}

// putChunks publishes dependencies before the root while excluding Sweep's
// removals; abandoned chunks after an error or crash are ordinary
// unreachable objects.
func (s *Store) putChunks(id OID, in io.Reader) error {
	s.ingest.RLock()
	defer s.ingest.RUnlock()
	if s.packs.touch(rawOf(id), time.Now().UnixNano()) {
		return nil
	}
	b := chunkBuilder{s: s, buf: make([]byte, 0, chunkMax)}
	bp := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(bp)
	for {
		n, err := in.Read(*bp)
		if n > 0 {
			if err := b.write((*bp)[:n]); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return b.finish(id)
		}
		if err != nil {
			return err
		}
	}
}

func (s *Store) chunkStage(id OID, stage string) error {
	defer removeFile(stage)
	in, err := os.Open(stage)
	if err != nil {
		return err
	}
	defer in.Close()
	return s.putChunks(id, in)
}

type chunkObject struct {
	id   rawID
	e    packed
	pf   *packFile
	refs []chunkRef
	ends []int64
	size int64
}

// chunkReader pins pack handles rather than blocking Sweep for its lifetime.
// Its graph contains each stored dependency once, even for repeated pages;
// corrupt manifests cannot expand exponentially into an unbounded leaf list.
type chunkReader struct {
	root   *chunkObject
	nodes  map[rawID]*chunkObject
	files  map[uint32]*packFile
	size   int64
	pos    int64
	closed atomic.Bool
	once   sync.Once
}

func (s *Store) openChunks(id rawID) (*chunkReader, error) {
	s.packs.appendMu.Lock()
	defer s.packs.appendMu.Unlock()
	r := &chunkReader{files: make(map[uint32]*packFile), nodes: make(map[rawID]*chunkObject)}
	active := make(map[rawID]bool)
	var walk func(rawID, int64, int, bool) (*chunkObject, error)
	walk = func(id rawID, want int64, depth int, root bool) (*chunkObject, error) {
		if depth > manifestDepth || active[id] {
			return nil, errCorrupt
		}
		if node := r.nodes[id]; node != nil {
			if node.size != want {
				return nil, errCorrupt
			}
			return node, nil
		}
		e, ok := s.packs.lookup(id)
		if !ok {
			return nil, fmt.Errorf("chunk dependency %s: %w", id.oid(), os.ErrNotExist)
		}
		node := &chunkObject{id: id, e: e}
		if e.flags&frameChunk != 0 && !root {
			if e.raw > chunkMax || int64(e.raw) != want {
				return nil, errCorrupt
			}
			pf := r.files[e.pack]
			if pf == nil {
				s.packs.mu.RLock()
				pf = s.packs.files[e.pack]
				if pf != nil {
					pf.refs.Add(1)
				}
				s.packs.mu.RUnlock()
				if pf == nil {
					return nil, errCorrupt
				}
				r.files[e.pack] = pf
			}
			node.pf, node.size = pf, want
			r.nodes[id] = node
			return node, nil
		}
		flags := uint8(frameManifest | frameInternal)
		if root {
			flags = frameManifest
		}
		if e.flags != flags {
			return nil, errCorrupt
		}
		_, frame, _, err := s.packs.frame(id)
		if err != nil {
			return nil, err
		}
		if !root && rawOf(HashBytes("manifest", frame[frameHeader:])) != id {
			return nil, errCorrupt
		}
		refs, size, err := parseManifest(frame[frameHeader:])
		if err != nil || !root && size != want {
			return nil, errCorrupt
		}
		node.refs, node.size = refs, size
		node.ends = make([]int64, len(refs))
		active[id] = true
		var end int64
		for i, ref := range refs {
			if _, err := walk(ref.id, ref.size, depth+1, false); err != nil {
				return nil, err
			}
			end += ref.size
			node.ends[i] = end
		}
		delete(active, id)
		r.nodes[id] = node
		return node, nil
	}
	root, err := walk(id, 0, 0, true)
	if err != nil {
		r.Close()
		return nil, err
	}
	r.root, r.size = root, root.size
	return r, nil
}

func (r *chunkReader) readNode(node *chunkObject, p []byte, off int64, depth int) (int, error) {
	if depth > manifestDepth {
		return 0, errCorrupt
	}
	if node.e.flags&frameChunk != 0 {
		frame := make([]byte, frameHeader+int(node.e.stored))
		if _, err := node.pf.f.ReadAt(frame, int64(node.e.off)); err != nil {
			return 0, err
		}
		if err := checkFrame(node.id, node.e, frame); err != nil {
			return 0, err
		}
		data, err := decodeFrame(node.e, frame)
		if err != nil || rawOf(HashBytes("chunk", data)) != node.id {
			return 0, errCorrupt
		}
		return copy(p, data[off:]), nil
	}
	n := 0
	for i := sort.Search(len(node.ends), func(i int) bool { return node.ends[i] > off }); n < len(p) && i < len(node.refs); i++ {
		ref := node.refs[i]
		start := node.ends[i] - ref.size
		limit := min(len(p)-n, int(node.ends[i]-off))
		k, err := r.readNode(r.nodes[ref.id], p[n:n+limit], off-start, depth+1)
		n += k
		off += int64(k)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ReadAt verifies and decodes overlapping chunks without changing the seek position.
func (r *chunkReader) ReadAt(p []byte, off int64) (int, error) {
	if r.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("negative object offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	n, err := r.readNode(r.root, p[:min(int64(len(p)), r.size-off)], off, 0)
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

// Read advances the seek position through verified chunk content.
func (r *chunkReader) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.pos)
	r.pos += int64(n)
	return n, err
}

// Seek permits positions past EOF but rejects negative offsets and unknown origins.
func (r *chunkReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += r.pos
	case io.SeekEnd:
		off += r.size
	default:
		return 0, fmt.Errorf("invalid seek origin")
	}
	if off < 0 {
		return 0, fmt.Errorf("negative object offset")
	}
	r.pos = off
	return off, nil
}

// Close releases pinned packs; subsequent reads return os.ErrClosed.
func (r *chunkReader) Close() error {
	r.once.Do(func() {
		r.closed.Store(true)
		for _, pf := range r.files {
			pf.release()
		}
	})
	return nil
}

func (s *Store) readChunks(id rawID) ([]byte, error) {
	r, err := s.openChunks(id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err == nil && rawOf(HashBytes("blob", data)) != id {
		err = errCorrupt
	}
	return data, err
}

func (s *Store) cloneChunks(id OID, dst string, mode os.FileMode) error {
	r, err := s.openChunks(rawOf(id))
	if err != nil {
		return err
	}
	defer r.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	h := newHash("blob", r.size)
	_, err = io.Copy(io.MultiWriter(out, h), r)
	if err == nil && sumOID(h) != id {
		err = errCorrupt
	}
	if err == nil {
		err = out.Chmod(mode)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
