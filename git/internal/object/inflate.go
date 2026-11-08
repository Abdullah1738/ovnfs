package object

import (
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sync"
)

// maxDeflateRatio bounds how much larger than its compressed form a zlib
// stream can inflate (deflate's limit is about 1032:1), so a corrupt size
// header cannot make a reader allocate far more than the data could hold.
const maxDeflateRatio = 1100

// maxInitialAlloc caps how much is allocated up front on the strength of a
// size declared by a pack entry, loose header or delta: such sizes are
// attacker-controlled in fetched packs, so larger buffers grow only as
// output is actually produced.
const maxInitialAlloc = 16 << 20

// zlibPool recycles decompressors: each holds tens of kilobytes of window
// and tables that would otherwise be allocated per object.
var zlibPool sync.Pool

// openZlib returns a pooled decompressor reading the zlib stream in r;
// release it with closeZlib.
func openZlib(r io.Reader) (io.ReadCloser, error) {
	if z, ok := zlibPool.Get().(io.ReadCloser); ok {
		if err := z.(zlib.Resetter).Reset(r, nil); err != nil {
			return nil, err
		}
		return z, nil
	}
	return zlib.NewReader(r)
}

func closeZlib(z io.ReadCloser) {
	z.Close()
	zlibPool.Put(z)
}

// inflate decompresses exactly size bytes from the zlib stream in r and
// requires the stream, including its checksum, to end there.
func inflate(r io.Reader, size int64) ([]byte, error) {
	z, err := openZlib(r)
	if err != nil {
		return nil, fmt.Errorf("zlib: %w", err)
	}
	defer closeZlib(z)
	buf, err := readSized(z, size)
	if err != nil {
		return nil, fmt.Errorf("zlib: inflating %d bytes: %w", size, err)
	}
	if err := expectEnd(z); err != nil {
		return nil, err
	}
	return buf, nil
}

// readSized reads exactly size bytes from r into a buffer that starts at no
// more than maxInitialAlloc and grows with the data actually read.
func readSized(r io.Reader, size int64) ([]byte, error) {
	if size < 0 || uint64(size) > math.MaxInt {
		return nil, fmt.Errorf("size %d too large", size)
	}
	want := int(size)
	buf := make([]byte, 0, min(want, maxInitialAlloc))
	for len(buf) < want {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, min(want-len(buf), len(buf)))
		}
		n, err := r.Read(buf[len(buf):min(cap(buf), want)])
		buf = buf[:len(buf)+n]
		if err != nil && len(buf) < want {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
	return buf, nil
}

// expectEnd checks that a zlib stream has no data left and a valid checksum.
func expectEnd(z io.Reader) error {
	var b [1]byte
	for {
		n, err := z.Read(b[:])
		if n > 0 {
			return errors.New("zlib: stream longer than its declared size")
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("zlib: %w", err)
		}
	}
}
