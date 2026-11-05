package remotefs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Frame types of the v1 wire (vibevmm vmm/src/virtio/remotefs.rs proto::REQ…REQ_DATA).
const (
	// FrameReq carries one JSON request.
	FrameReq byte = 1
	// FrameOKJSON carries a JSON reply.
	FrameOKJSON byte = 2
	// FrameOKData carries the raw bytes of a read reply.
	FrameOKData byte = 3
	// FrameErr carries a JSON ErrReply.
	FrameErr byte = 4
	// FrameReqData carries a u32 LE JSON length, the JSON request, then raw bytes (writes).
	FrameReqData byte = 5
)

// HeaderLen is the size of a frame header: payload length u32 LE, type u8, id u32 LE.
const HeaderLen = 9

// MaxFrame is the largest payload either side accepts (2 MiB).
const MaxFrame = 2 << 20

// MaxRead is the largest read the server answers; the VMM never asks for more (its FUSE max_write).
const MaxRead = 1 << 20

// errFrameTooLarge reports a payload over MaxFrame.
var errFrameTooLarge = errors.New("remotefs: frame payload exceeds 2 MiB")

// readFrame reads one frame into buf (grown as needed) and returns its type, id and payload.
func readFrame(r io.Reader, buf []byte) (ty byte, id uint32, payload []byte, err error) {
	var header [HeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, 0, buf, err
	}
	n := binary.LittleEndian.Uint32(header[:4])
	if n > MaxFrame {
		return 0, 0, buf, fmt.Errorf("%w: %d bytes", errFrameTooLarge, n)
	}
	if cap(buf) < int(n) {
		buf = make([]byte, n)
	}
	payload = buf[:n]
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, 0, payload, fmt.Errorf("remotefs: read frame payload: %w", err)
	}
	return header[4], binary.LittleEndian.Uint32(header[5:]), payload, nil
}

// appendFrame appends a frame with the given type, id and payload parts to dst.
func appendFrame(dst []byte, ty byte, id uint32, parts ...[]byte) ([]byte, error) {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	if n > MaxFrame {
		return dst, fmt.Errorf("%w: %d bytes", errFrameTooLarge, n)
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(n))
	dst = append(dst, ty)
	dst = binary.LittleEndian.AppendUint32(dst, id)
	for _, p := range parts {
		dst = append(dst, p...)
	}
	return dst, nil
}

// splitData separates a FrameReqData payload into its JSON request and data.
func splitData(payload []byte) (json, data []byte, err error) {
	if len(payload) < 4 {
		return nil, nil, errors.New("remotefs: data request shorter than its length prefix")
	}
	n := uint64(binary.LittleEndian.Uint32(payload))
	if n > uint64(len(payload)-4) {
		return nil, nil, fmt.Errorf("remotefs: data request JSON length %d exceeds payload", n)
	}
	return payload[4 : 4+n], payload[4+n:], nil
}
