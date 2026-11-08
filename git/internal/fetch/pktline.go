package fetch

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The pkt-line framing of gitprotocol-common: a four hex digit length that
// counts itself, then the payload. Lengths 0-2 are the special packets of
// protocol v2; 3 is never valid.
const (
	maxPacketPayload = 65516 // the largest payload a pkt-line may carry
	maxPacketLen     = maxPacketPayload + 4
)

// packetKind tells data packets from the special zero-payload packets.
type packetKind uint8

const (
	pktData packetKind = iota
	pktFlush
	pktDelim       // "0001", separates sections of a v2 message
	pktResponseEnd // "0002", ends a v2 response on stateless connections
)

func (k packetKind) String() string {
	switch k {
	case pktFlush:
		return "flush-pkt"
	case pktDelim:
		return "delim-pkt"
	case pktResponseEnd:
		return "response-end-pkt"
	}
	return "data-pkt"
}

// errProtocol marks a malformed or unexpected server response.
var errProtocol = errors.New("protocol error")

// remoteError is an error the server reported: an "ERR" packet or a message
// on side-band 3.
type remoteError struct{ msg string }

func (e *remoteError) Error() string { return "remote error: " + e.msg }

// pktReader reads pkt-lines from a stream.
type pktReader struct {
	r   *bufio.Reader
	buf [maxPacketLen]byte
	hdr [4]byte
	// unread holds a packet pushed back by unreadPacket.
	unread    bool
	unreadPkt packetKind
	unreadLen int
}

// unreadPacket makes the next readPacket return the packet just read again.
func (p *pktReader) unreadPacket(kind packetKind, data []byte) {
	p.unread, p.unreadPkt, p.unreadLen = true, kind, len(data)
}

func newPktReader(r io.Reader) *pktReader {
	return &pktReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// readPacket returns the next packet. The payload aliases an internal buffer
// valid until the next call. A clean end of stream before a packet starts is
// io.EOF; one inside a packet is io.ErrUnexpectedEOF.
func (p *pktReader) readPacket() (packetKind, []byte, error) {
	if p.unread {
		// The payload is still in buf: nothing was read since.
		p.unread = false
		return p.unreadPkt, p.buf[:p.unreadLen], nil
	}
	if _, err := io.ReadFull(p.r, p.hdr[:]); err != nil {
		return 0, nil, err
	}
	n, ok := parseHexLen(p.hdr[:])
	if !ok {
		return 0, nil, fmt.Errorf("%w: bad pkt-line length %q", errProtocol, p.hdr[:])
	}
	switch {
	case n == 0:
		return pktFlush, nil, nil
	case n == 1:
		return pktDelim, nil, nil
	case n == 2:
		return pktResponseEnd, nil, nil
	case n < 4 || n > maxPacketLen:
		return 0, nil, fmt.Errorf("%w: bad pkt-line length %d", errProtocol, n)
	}
	data := p.buf[:n-4]
	if _, err := io.ReadFull(p.r, data); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return pktData, data, nil
}

// readLine returns the next packet with a text payload: the trailing LF is
// stripped, and an "ERR <msg>" packet, which a server may send in place of
// any line, becomes a remoteError. Special packets have an empty line.
func (p *pktReader) readLine() (packetKind, string, error) {
	kind, data, err := p.readPacket()
	if err != nil || kind != pktData {
		return kind, "", err
	}
	line := strings.TrimSuffix(string(data), "\n")
	if msg, ok := strings.CutPrefix(line, "ERR "); ok {
		return 0, "", &remoteError{msg: msg}
	}
	return pktData, line, nil
}

func parseHexLen(b []byte) (int, bool) {
	n := 0
	for _, c := range b {
		var d byte
		switch {
		case '0' <= c && c <= '9':
			d = c - '0'
		case 'a' <= c && c <= 'f':
			d = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		n = n<<4 | int(d)
	}
	return n, true
}

// pktWriter buffers pkt-lines for one request. A payload over the limit
// sets err, which sticks; the caller checks it once at the end.
type pktWriter struct {
	buf []byte
	err error
}

const hexDigits = "0123456789abcdef"

// data appends a packet carrying payload; one over the limit fails the
// request instead.
func (w *pktWriter) data(payload []byte) {
	if w.err != nil {
		return
	}
	if len(payload) > maxPacketPayload {
		w.err = fmt.Errorf("fetch: pkt-line payload of %d bytes exceeds the %d-byte limit", len(payload), maxPacketPayload)
		return
	}
	n := len(payload) + 4
	w.buf = append(w.buf, hexDigits[n>>12&0xf], hexDigits[n>>8&0xf], hexDigits[n>>4&0xf], hexDigits[n&0xf])
	w.buf = append(w.buf, payload...)
}

// line appends a text packet: the formatted string followed by LF.
func (w *pktWriter) line(format string, args ...any) {
	w.data([]byte(fmt.Sprintf(format, args...) + "\n"))
}

func (w *pktWriter) flush() { w.buf = append(w.buf, "0000"...) }
func (w *pktWriter) delim() { w.buf = append(w.buf, "0001"...) }

// sidebandReader demultiplexes a side-band-64k stream: band 1 is returned as
// data, band 2 (progress) is discarded, band 3 aborts with the server's
// message. A flush packet ends the stream.
type sidebandReader struct {
	p    *pktReader
	rest []byte // undelivered band 1 data of the current packet
	err  error
}

func (s *sidebandReader) Read(b []byte) (int, error) {
	for len(s.rest) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		kind, data, err := s.p.readPacket()
		switch {
		case err == io.EOF:
			s.err = fmt.Errorf("%w: pack stream ended without a flush-pkt", io.ErrUnexpectedEOF)
		case err != nil:
			s.err = err
		case kind == pktFlush:
			s.err = io.EOF
		case kind != pktData:
			s.err = fmt.Errorf("%w: unexpected %v in pack stream", errProtocol, kind)
		case len(data) == 0:
			s.err = fmt.Errorf("%w: empty side-band packet", errProtocol)
		default:
			switch data[0] {
			case 1:
				s.rest = data[1:]
			case 2:
				// Progress, including the v2 keepalive "0005\2".
			case 3:
				s.err = &remoteError{msg: strings.TrimRight(string(data[1:]), "\n")}
			default:
				s.err = fmt.Errorf("%w: bad side-band %d", errProtocol, data[0])
			}
		}
	}
	n := copy(b, s.rest)
	s.rest = s.rest[n:]
	return n, nil
}
