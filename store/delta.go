package store

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// deltaMax is the largest blob PutFileLike stores as a delta: reading one
// rebuilds its whole content in memory.
const deltaMax = 16 << 20

// deltaDepth bounds delta chains, so reading an object decodes at most this many deltas.
const deltaDepth = 8

// deltaMinWindow is the smallest zstd window of a delta frame. A frame's window
// spans its base, used as dictionary, plus the new content: up to 2*deltaMax.
const deltaMinWindow = 256 << 10

// deltaClasses counts the window sizes delta encoders use, deltaMinWindow
// doubling up to 2*deltaMax.
const deltaClasses = 8

// deltaEncoders recycles single-goroutine encoders, which keep their match
// tables between uses, per window class: an encoder's memory grows with its
// window (a 32 MiB one holds over 100 MB), and the window cannot change on
// reset. Each use installs its own dictionary, and releases a large one
// afterwards (see releaseDict).
var deltaEncoders [deltaClasses]sync.Pool

// deltaEncoder returns a pooled encoder whose window covers span bytes and the
// pool to return it to.
func deltaEncoder(span int) (*zstd.Encoder, *sync.Pool, error) {
	class := 0
	for deltaMinWindow<<class < span && class < deltaClasses-1 {
		class++
	}
	pool := &deltaEncoders[class]
	if e, ok := pool.Get().(*zstd.Encoder); ok {
		return e, pool, nil
	}
	// The default level's tables (about 12 MB with a dictionary) find matches in
	// bases up to about a megabyte; larger ones need the better matcher's
	// (about 24 MB), or a small edit to a 3 MiB file costs 200 KiB instead of 4.
	level := zstd.SpeedDefault
	if deltaMinWindow<<class > deltaDefaultSpan {
		level = zstd.SpeedBetterCompression
	}
	e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(deltaMinWindow<<class))
	return e, pool, err
}

// deltaDefaultSpan is the largest window delta encoders use the default level for.
const deltaDefaultSpan = 1 << 20

// deltaDecoders recycles single-goroutine decoders; each use installs its own dictionary.
var deltaDecoders = sync.Pool{New: func() any {
	d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecodeAllCapLimit(true), zstd.WithDecoderMaxMemory(2*deltaMax))
	if err != nil {
		panic(err) // constant options
	}
	return d
}}

// encodeDelta returns data as a delta frame for id compressed against dict,
// the content of base, fresh at at.
func encodeDelta(id rawID, data []byte, base rawID, dict []byte, at int64) ([]byte, error) {
	enc, pool, err := deltaEncoder(len(dict) + len(data))
	if err != nil {
		return nil, err
	}
	defer pool.Put(enc)
	if err := enc.ResetWithOptions(nil, zstd.WithEncoderDictRaw(0, dict)); err != nil {
		return nil, err
	}
	buf := make([]byte, frameHeader+len(base))
	copy(buf[frameHeader:], base[:])
	buf = enc.EncodeAll(data, buf)
	if len(dict) > deltaDefaultSpan {
		releaseDict(enc)
	}
	sealFrame(buf, id, frameZstd|frameDelta, len(data), at)
	return buf, nil
}

// releaseDict makes enc let go of its dictionary's content, the whole of a
// base, which a pooled encoder would otherwise keep alive until its next use.
// encodeDelta leaves bases up to deltaDefaultSpan in place: they are smaller
// than the encoder's own tables, and releasing one costs two table resets,
// over half the time a small delta takes to encode.
//
// zstd.WithEncoderDictDelete does not release the content: it discards the
// tables and history the pool exists to keep, while the encoder EncodeAll ran
// on still references the dictionary. Both encoders inside enc drop one only
// when reset with another, which EncodeAll does for its own given any input.
func releaseDict(enc *zstd.Encoder) {
	enc.ResetWithOptions(nil, zstd.WithEncoderDictRaw(0, nil)) // fails only for oversized content
	var frame [32]byte
	enc.EncodeAll(noDict, frame[:0])
}

// noDict is the input releaseDict encodes against the empty dictionary.
var noDict = []byte{0}

// decodeDelta rebuilds the content of object id from its checked delta frame
// and the content of its base, verifying it hashes to id.
func (s *Store) decodeDelta(id rawID, e packed, frame []byte, depth int) ([]byte, error) {
	if depth >= deltaDepth {
		return nil, errCorrupt
	}
	base := deltaBase(frame)
	dict, ok := s.cache.get(base.oid())
	if !ok {
		var err error
		if dict, err = s.loadDepth(base.oid(), depth+1); err != nil {
			return nil, fmt.Errorf("delta base %s: %w", base.oid(), err)
		}
	}
	d := deltaDecoders.Get().(*zstd.Decoder)
	defer deltaDecoders.Put(d)
	if err := d.ResetWithOptions(nil, zstd.WithDecoderDictRaw(0, dict)); err != nil {
		return nil, err
	}
	data, err := d.DecodeAll(frame[frameHeader+len(base):], make([]byte, 0, e.raw))
	// Drop the dictionary so the pooled decoder does not pin the base's content.
	d.ResetWithOptions(nil, zstd.WithDecoderDictDelete())
	kind := "blob"
	if e.flags&frameTree != 0 {
		kind = "tree"
	}
	if err != nil || len(data) != int(e.raw) || rawOf(HashBytes(kind, data)) != id {
		return nil, errCorrupt
	}
	return data, nil
}

// PutFileLike stores a regular file as PutFile does, but may record it as a
// delta against like, a stored blob with similar content (zstd with like's
// content as raw dictionary), so a small edit to a large file costs bytes
// proportional to the edit. Snapshot hashing calls it with the blob a path
// had in the parent snapshot. A delta is used for files up to deltaMax bytes
// when it halves storage. Large copy-only files use content-defined chunks.
// A full chain is rebased against its original full object at rollover.
func (s *Store) PutFileLike(path string, like OID) (OID, int64, error) {
	if !like.Valid() || !s.Has(like) {
		return s.PutFile(path)
	}
	if e, ok := s.packs.lookup(rawOf(like)); ok && e.flags == frameManifest {
		return s.putFile(path, true)
	}
	if size, err := s.Size(like); err != nil || size > deltaMax {
		return s.PutFile(path)
	}
	in, size, dev, err := openRegular(path)
	if err != nil {
		return "", 0, fmt.Errorf("store %s: %w", path, err)
	}
	_, reflink := s.reflink.Load(dev)
	_, noReflink := s.noReflink.Load(dev)
	if size > deltaMax || size > cloneFile && (!reflink || noReflink) {
		in.Close()
		return s.PutFile(path)
	}
	data, whole, err := readSmall(in, make([]byte, size+1))
	in.Close()
	if err != nil {
		return "", 0, fmt.Errorf("store %s: %w", path, err)
	}
	if !whole {
		// The file grew while it was read.
		return s.PutFile(path)
	}
	id, err := s.putLike(data, like)
	if err != nil {
		return "", 0, fmt.Errorf("store %s: %w", path, err)
	}
	return id, int64(len(data)), nil
}

// putLike stores data as a blob, as a delta against like when that pays off.
func (s *Store) putLike(data []byte, like OID) (OID, error) {
	return s.putLikeKind("blob", data, like)
}

// putLikeKind verifies the base's hash domain before encoding a typed delta.
func (s *Store) putLikeKind(kind string, data []byte, like OID) (OID, error) {
	id := HashBytes(kind, data)
	tree := kind == "tree"
	if s.touch(id, nil, tree) {
		return id, nil
	}
	plainObject := func() (OID, error) { return s.putData(kind, id, data) }
	if !like.Valid() || len(data) > deltaMax {
		return plainObject()
	}
	if s.packs.depth(rawOf(like)) >= deltaDepth {
		root, ok := s.packs.chainRoot(rawOf(like))
		if !ok {
			return plainObject()
		}
		like = root.oid()
	}
	if size, err := s.Size(like); err != nil || size > deltaMax {
		return plainObject()
	}
	dict, ok := s.cache.get(like)
	if !ok {
		var err error
		if dict, err = s.load(like); err != nil {
			// like was swept meanwhile: store data on its own.
			return plainObject()
		}
	}
	if HashBytes(kind, dict) != like {
		return plainObject()
	}
	at := time.Now().UnixNano()
	delta, err := encodeDelta(rawOf(id), data, rawOf(like), dict, at)
	if err != nil {
		return "", err
	}
	if tree {
		sealFrame(delta, rawOf(id), delta[24]|frameTree, len(data), at)
	}
	stored := len(delta) - frameHeader
	var plain []byte
	if len(data) <= cloneFile {
		plain = encodeFrame(rawOf(id), data, at, 0)
		if stored > (len(plain)-frameHeader)/2 {
			return id, s.putFrame(id, plain, nil, tree)
		}
	} else if stored > cloneFile || stored > len(data)/2 {
		return plainObject()
	}
	packed, err := s.putDelta(delta, like, id, tree)
	if err != nil || packed {
		return id, err
	}
	if plain != nil {
		return id, s.putFrame(id, plain, nil, tree)
	}
	return plainObject()
}

// putDelta packs a delta frame while its base like is still stored (Sweep is
// held off meanwhile, and afterwards keeps the base for the delta), reporting
// false when like was removed since its content was read.
func (s *Store) putDelta(frame []byte, like, id OID, tree bool) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.Has(like) || s.packs.depth(rawOf(like)) >= deltaDepth {
		return false, nil
	}
	if _, err := s.packs.add(frame); err != nil {
		return false, err
	}
	s.stored(id, nil, tree)
	return true, nil
}

// readPacked returns the content of packed object id in a new slice (a delta
// rebuilt from its base); ok is false when id is not packed.
func (s *Store) readPacked(id rawID) ([]byte, bool, error) {
	return s.readPackedDepth(id, 0)
}

func (s *Store) readPackedDepth(id rawID, depth int) ([]byte, bool, error) {
	e, frame, ok, err := s.packs.frame(id)
	if !ok || err != nil {
		return nil, ok, err
	}
	var data []byte
	if e.flags&frameManifest != 0 {
		data, err = s.readChunks(id)
	} else if e.flags&frameChunk != 0 {
		return nil, true, errCorrupt
	} else if e.flags&frameDelta == 0 {
		data, err = decodeFrame(e, frame)
	} else {
		data, err = s.decodeDelta(id, e, frame, depth)
	}
	if err != nil {
		return nil, true, fmt.Errorf("pack %d offset %d: %w", e.pack, e.off, err)
	}
	return data, true, nil
}

// load reads stored object id's content from its pack or loose file,
// bypassing the cache, into a new slice.
func (s *Store) load(id OID) ([]byte, error) {
	return s.loadDepth(id, 0)
}

func (s *Store) loadDepth(id OID, depth int) ([]byte, error) {
	data, ok, err := s.readPackedDepth(rawOf(id), depth)
	if !ok && err == nil {
		data, err = os.ReadFile(s.objectPath(id))
	}
	return data, err
}
