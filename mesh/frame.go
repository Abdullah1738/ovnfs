package mesh

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// WriteFrame writes p as one frame: its length as a little-endian u32, then
// p, in a single write.
func WriteFrame(w io.Writer, p []byte) error {
	if uint64(len(p)) > math.MaxUint32 {
		return fmt.Errorf("mesh: frame of %d bytes exceeds u32", len(p))
	}
	buf := make([]byte, 4+len(p))
	binary.LittleEndian.PutUint32(buf, uint32(len(p)))
	copy(buf[4:], p)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame of at most max bytes, refusing a longer one
// before reading its payload.
func ReadFrame(r io.Reader, max int) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(prefix[:])
	if max < 0 || uint64(n) > uint64(max) {
		return nil, fmt.Errorf("mesh: frame of %d bytes exceeds %d", n, max)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return p, nil
}
