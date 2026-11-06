// Package peer is the link protocol that attaches a checkout on one machine to
// another: the source machine offers a snapshot (every leaf with its git id),
// the remote machine accepts it and then fetches blob content lazily, verifying
// each blob by its id before it enters the remote's store.
//
// A link is a single full-duplex byte stream (typically ssh stdio), so
// everything travels as frames:
//
//	[type u8][length u32 big-endian][payload]
//
// The header layout never changes between protocol versions, so a mismatch is
// always reported rather than misparsed. Frames, by direction:
//
//	source→remote 'O' offer:   "OVNL" version(u32) gzip(JSON Offer); first frame, ≤64 MiB
//	remote→source 'A' answer:  "OVNL" version(u32) JSON {accepted|error}; first frame
//	remote→source 'R' request: repeated tag(u32) id(20 raw bytes) len(uvarint) path
//	source→remote 'B' begin:   tag(u32) size(u64) first chunk
//	source→remote 'D' data:    tag(u32) chunk (≤256 KiB)
//	source→remote 'X' error:   tag(u32) message; the blob is abandoned
//	either        'Q' goodbye: empty; the sender closes cleanly after it
//
// Source-initiated pulls use 'P' (tag and base tree id) and 'K' (cancel tag).
// The remote replies with tagged 'M' metadata chunks, 'N' metadata end,
// 'J' blob headers (20-byte id and u64 size), 'L' blob chunks and 'E' completion,
// or 'F' with an error message. Metadata is JSON Pulled without Data; blobs
// travel as raw bytes, each chunk at most 256 KiB. Pulls and fetches interleave.
//
// Tags are chosen by the remote, one per requested blob. The source streams up
// to four blobs at once, interleaving their chunks; a blob is complete once
// its announced size has arrived. Every frame other than the offer is capped
// at 1 MiB.
//
// Remote searches scan blobs where their bytes are, moving only results:
//
//	remote→source 'S' search: tag(u32) len(uvarint) JSON grep.Query, then repeated id(20 raw bytes) len(uvarint) path
//	remote→source 'C' cancel: tag(u32); the source stops and sends nothing more for it
//	source→remote 'H' hits:   tag(u32) chunk (≤256 KiB) of JSON []grep.Outcome, one per id in request order
//	source→remote 'Z' done:   tag(u32) [error message]; without a message the hits are complete
//
// Search tags are chosen by the remote, one per request frame, independent of
// blob tags. The source scans up to four requests at once; a blob it cannot
// produce comes back unsearched; nothing a search reads enters the remote's store.
package peer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
)

// Version is the link protocol version; both sides refuse others.
const Version = 4

// Entry is one leaf of an offered snapshot (blob, symlink, or empty dir as store.ModeDir/store.EmptyTree).
type Entry struct {
	Path  string    `json:"p"`
	Mode  uint32    `json:"m"`
	ID    store.OID `json:"o"`
	Size  int64     `json:"s"`
	Mtime int64     `json:"t"` // unix nanoseconds
}

// Offer is what the source machine sends first: everything the remote needs to show the snapshot before any content moves.
type Offer struct {
	Node    string                 `json:"node"`   // sending daemon's stable id
	Source  string                 `json:"source"` // display, e.g. "laptop:/src/app"
	Name    string                 `json:"name"`   // checkout name wanted on the remote ("" = derive)
	Tree    store.OID              `json:"tree"`
	Lock    manifest.Lock          `json:"lock"`
	Files   []Entry                `json:"files"` // sorted by path
	Roots   map[string]rules.Class `json:"roots"`
	Created time.Time              `json:"created"`
	// Origins are commits on the source's git upstreams. A remote that can
	// fetch them reads committed content from there and asks this link only
	// for the rest; older remotes ignore them.
	Origins []git.Origin `json:"origins,omitempty"`
}

// Pulled contains a remote snapshot's source changes since the offered tree.
// Base and Files contain only changed paths; a missing entry means absence.
// Data contains every non-directory blob referenced by either map.
type Pulled struct {
	Snapshot api.Snapshot          `json:"snapshot"`
	Base     map[string]store.File `json:"base"`
	Files    map[string]store.File `json:"files"`
	Data     map[store.OID][]byte  `json:"data"`
}

// Accepted is the remote side's answer to an offer.
type Accepted struct {
	Checkout string `json:"checkout"`
	Path     string `json:"path"` // where the checkout appears on the remote machine
	Host     string `json:"host"`
}

// Frame types; see the package comment for their payloads.
const (
	frameOffer   byte = 'O'
	frameAnswer  byte = 'A'
	frameRequest byte = 'R'
	frameBegin   byte = 'B'
	frameData    byte = 'D'
	frameError   byte = 'X'
	frameBye     byte = 'Q'
	framePull    byte = 'P' // tag and base tree id
	frameCancel  byte = 'K' // pull tag
	frameMeta    byte = 'M' // tag and metadata chunk
	frameMetaEnd byte = 'N' // tag, ends metadata
	frameBlob    byte = 'J' // tag, blob id, size
	frameChunk   byte = 'L' // tag and blob chunk
	framePulled  byte = 'E' // tag, completes pull
	framePullErr byte = 'F' // tag and error message
	frameSearch  byte = 'S' // tag, query and wanted blobs
	frameAbort   byte = 'C' // search tag
	frameHits    byte = 'H' // tag and results chunk
	frameFound   byte = 'Z' // tag, ends results, optional error message
)

// Wire limits.
const (
	headerSize   = 5        // frame type and length
	maxOffer     = 64 << 20 // compressed offer frame
	maxOfferJSON = 1 << 30  // offer after decompression
	maxFrame     = 1 << 20  // any frame but the offer
	chunkSize    = 256 << 10
	requestBatch = 512 << 10 // request frames are split near this size
	maxPathHint  = 64 << 10  // longest path hint a request may carry
	maxMessage   = 4 << 10   // longest error message sent over the link
	maxQuery     = 256 << 10 // longest search query JSON
	maxResults   = 1 << 30   // longest results JSON of one search request
	streams      = 4         // blobs the source streams at once
	searchers    = 4         // search requests the source scans at once
	byeTimeout   = time.Second
)

// magic opens both handshake payloads so a stream that is not a link (say, a shell banner) is named as such.
var magic = []byte("OVNL")

// errBye marks a read loop that ended because the other side said goodbye.
var errBye = errors.New("goodbye")

// errClosed is what waiters see when the link ended cleanly under them.
var errClosed = errors.New("link closed")

// frame builds one frame of typ whose payload is the concatenation of parts.
func frame(typ byte, parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, headerSize, headerSize+n)
	for _, p := range parts {
		b = append(b, p...)
	}
	seal(b, typ)
	return b
}

// seal fills in the header of b, a complete frame whose first headerSize bytes are reserved.
func seal(b []byte, typ byte) {
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:headerSize], uint32(len(b)-headerSize))
}

// frameReader reads frames from a stream, reusing one buffer for ordinary frames.
type frameReader struct {
	r   *bufio.Reader
	buf []byte
}

// newFrameReader reads frames from r.
func newFrameReader(r io.Reader) *frameReader {
	return &frameReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next reads one frame. The payload is valid until the next call. If want is
// nonzero, any other frame type is rejected before its payload is read. A
// stream ending between frames returns io.EOF, inside one io.ErrUnexpectedEOF.
func (fr *frameReader) next(want byte) (byte, []byte, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(fr.r, h[:]); err != nil {
		return 0, nil, err
	}
	typ, n := h[0], binary.BigEndian.Uint32(h[1:])
	if want != 0 && typ != want {
		return 0, nil, fmt.Errorf("not an ovnfs link stream: got frame %q, want %q", typ, want)
	}
	limit := uint32(maxFrame)
	if typ == frameOffer {
		limit = maxOffer
	}
	if n > limit {
		return 0, nil, fmt.Errorf("frame %q of %d bytes exceeds the %d-byte limit", typ, n, limit)
	}
	var p []byte
	if n > maxFrame {
		p = make([]byte, n) // a large offer is read once; do not keep its buffer
	} else {
		if cap(fr.buf) < int(n) {
			fr.buf = make([]byte, maxFrame)
		}
		p = fr.buf[:n]
	}
	if _, err := io.ReadFull(fr.r, p); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return typ, p, nil
}

// hello is the version header that opens both handshake payloads.
func hello() []byte {
	return binary.BigEndian.AppendUint32(append([]byte(nil), magic...), Version)
}

// checkHello strips the version header from a handshake payload sent by who, failing on a foreign stream or another version.
func checkHello(p []byte, who string) ([]byte, error) {
	if len(p) < len(magic)+4 || !bytes.Equal(p[:len(magic)], magic) {
		return nil, errors.New("not an ovnfs link stream: missing protocol header")
	}
	if v := binary.BigEndian.Uint32(p[len(magic):]); v != Version {
		return nil, fmt.Errorf("%s speaks link protocol version %d, this side speaks version %d", who, v, Version)
	}
	return p[len(magic)+4:], nil
}

// answer is the remote's first frame: exactly one of its fields is set.
type answer struct {
	Accepted *Accepted `json:"accepted,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// encodeOffer compresses an offer into a handshake payload.
func encodeOffer(o Offer) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write(hello())
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(o); err != nil {
		return nil, fmt.Errorf("encode offer: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("encode offer: %w", err)
	}
	if buf.Len() > maxOffer {
		return nil, fmt.Errorf("encode offer: %d compressed bytes exceed the %d-byte limit", buf.Len(), maxOffer)
	}
	return buf.Bytes(), nil
}

// decodeOffer decompresses and checks an offer body (the payload after the version header).
func decodeOffer(body []byte) (Offer, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return Offer{}, fmt.Errorf("decode offer: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(zr, maxOfferJSON+1))
	if err != nil {
		return Offer{}, fmt.Errorf("decode offer: %w", err)
	}
	if len(data) > maxOfferJSON {
		return Offer{}, fmt.Errorf("decode offer: more than %d bytes uncompressed", maxOfferJSON)
	}
	var o Offer
	if err := json.Unmarshal(data, &o); err != nil {
		return Offer{}, fmt.Errorf("decode offer: %w", err)
	}
	if err := o.check(); err != nil {
		return Offer{}, fmt.Errorf("decode offer: %w", err)
	}
	return o, nil
}

// check rejects an offer the remote could not show safely: malformed ids, unknown modes, or paths escaping the checkout.
func (o *Offer) check() error {
	if !o.Tree.Valid() {
		return fmt.Errorf("malformed tree id %q", o.Tree)
	}
	for _, e := range o.Files {
		if !fs.ValidPath(e.Path) || e.Path == "." {
			return fmt.Errorf("invalid path %q", e.Path)
		}
		switch e.Mode {
		case store.ModeFile, store.ModeExec, store.ModeSymlink:
			if !e.ID.Valid() {
				return fmt.Errorf("%s: malformed id %q", e.Path, e.ID)
			}
			if e.Size < 0 {
				return fmt.Errorf("%s: negative size %d", e.Path, e.Size)
			}
		case store.ModeDir:
			if e.ID != store.EmptyTree {
				return fmt.Errorf("%s: directory leaf is not the empty tree", e.Path)
			}
		default:
			return fmt.Errorf("%s: unknown mode %o", e.Path, e.Mode)
		}
	}
	for _, origin := range o.Origins {
		if err := origin.Valid(); err != nil {
			return err
		}
	}
	return nil
}

// clip bounds an error message sent over the link.
func clip(msg string) []byte {
	if len(msg) > maxMessage {
		msg = msg[:maxMessage]
	}
	return []byte(msg)
}

// request is one wanted blob on the wire.
type request struct {
	tag  uint32
	id   store.OID
	path string
}

// encodeRequests packs requests into as many request frames as their size needs.
func encodeRequests(reqs []request) [][]byte {
	var frames [][]byte
	b := make([]byte, headerSize, 4096)
	for _, r := range reqs {
		raw, _ := hexID(r.id)
		b = binary.BigEndian.AppendUint32(b, r.tag)
		b = append(b, raw[:]...)
		b = binary.AppendUvarint(b, uint64(len(r.path)))
		b = append(b, r.path...)
		if len(b) >= requestBatch {
			seal(b, frameRequest)
			frames = append(frames, b)
			b = make([]byte, headerSize, 4096)
		}
	}
	if len(b) > headerSize {
		seal(b, frameRequest)
		frames = append(frames, b)
	}
	return frames
}

// decodeRequests unpacks a request frame's payload.
func decodeRequests(p []byte) ([]request, error) {
	var reqs []request
	for len(p) > 0 {
		if len(p) < 24 {
			return nil, errors.New("truncated request")
		}
		r := request{tag: binary.BigEndian.Uint32(p), id: store.OID(fmt.Sprintf("%x", p[4:24]))}
		p = p[24:]
		n, k := binary.Uvarint(p)
		if k <= 0 || n > uint64(len(p)-k) {
			return nil, errors.New("truncated request path")
		}
		r.path = string(p[k : k+int(n)])
		p = p[k+int(n):]
		reqs = append(reqs, r)
	}
	return reqs, nil
}

// hexID converts a valid id to its 20 raw bytes.
func hexID(id store.OID) ([20]byte, bool) {
	var raw [20]byte
	if !id.Valid() {
		return raw, false
	}
	for i := range raw {
		raw[i] = unhex(id[2*i])<<4 | unhex(id[2*i+1])
	}
	return raw, true
}

// unhex is the value of one lowercase hex digit.
func unhex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}
