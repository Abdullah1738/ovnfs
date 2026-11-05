package smb

import (
	"encoding/binary"
	"os"
	"strings"
	"sync"
)

// RPC constants describe connection-oriented DCE/RPC with the NDR20 transfer syntax.
const (
	rpcRequest                     = 0
	rpcResponse                    = 2
	rpcFault                       = 3
	rpcBind                        = 11
	rpcBindAck                     = 12
	rpcBindNak                     = 13
	rpcAlterContext                = 14
	rpcAlterContextResponse        = 15
	rpcFirst                       = 1
	rpcLast                        = 2
	rpcOpRangeError                = 0x1c010002
	rpcUnknownInterface            = 0x1c010003
	rpcProtocolError               = 0x1c01000b
	rpcBadStub                     = 0x1c000006
	statusPipeEmpty         uint32 = 0xc00000d9
)

// srvsvcSyntax is the wire-order SRVS interface UUID followed by version 3.0.
var srvsvcSyntax = [20]byte{0xc8, 0x4f, 0x32, 0x4b, 0x70, 0x16, 0xd3, 0x01, 0x12, 0x78, 0x5a, 0x47, 0xbf, 0x6e, 0xe1, 0x88, 3, 0, 0, 0}

// ndr20Syntax is the little-endian NDR transfer-syntax identifier and version 2.0.
var ndr20Syntax = [20]byte{0x04, 0x5d, 0x88, 0x8a, 0xeb, 0x1c, 0xc9, 0x11, 0x9f, 0xe8, 0x08, 0x00, 0x2b, 0x10, 0x48, 0x60, 2, 0, 0, 0}

// pipe holds buffered RPC bytes, negotiated presentation contexts, and response fragments.
type pipe struct {
	mu        sync.Mutex
	srv       *Server
	name      string
	input     []byte
	output    []byte
	contexts  map[uint16]bool
	maxRecv   int
	calls     map[uint32]*rpcCall
	callBytes int
}

// rpcCall accumulates a fragmented request while preserving its presentation context and operation.
type rpcCall struct {
	context uint16
	op      uint16
	stub    []byte
}

// openPipe creates an IPC endpoint without exposing arbitrary host named pipes.
func openPipe(name string) (*pipe, uint32) {
	name = strings.ToLower(strings.ReplaceAll(name, "/", "\\"))
	name = strings.Trim(name, "\\")
	name = strings.TrimPrefix(name, "pipe\\")
	if name != "srvsvc" && name != "wkssvc" {
		return nil, statusObjectNameNotFound
	}
	return &pipe{name: name, maxRecv: 4280, contexts: make(map[uint16]bool), calls: make(map[uint32]*rpcCall)}, statusSuccess
}

// write accepts partial or concatenated RPC PDUs and queues each completed call's response.
func (p *pipe) write(b []byte) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.writeLocked(b)
}

// writeLocked reassembles PDUs under the pipe lock with a finite per-pipe memory bound.
func (p *pipe) writeLocked(b []byte) uint32 {
	if len(b) > 16<<20 || len(p.input)+len(b) > 16<<20 || len(p.output) > 16<<20 {
		return statusInsufficientResources
	}
	p.input = append(p.input, b...)
	for len(p.input) >= 16 {
		h := p.input
		if h[0] != 5 || h[1] != 0 || h[4] != 0x10 || h[5] != 0 || h[6] != 0 || h[7] != 0 {
			p.input = nil
			return statusInvalidParameter
		}
		n := int(binary.LittleEndian.Uint16(h[8:]))
		if n < 16 {
			p.input = nil
			return statusInvalidParameter
		}
		if len(h) < n {
			break
		}
		call := binary.LittleEndian.Uint32(h[12:])
		auth := int(binary.LittleEndian.Uint16(h[10:]))
		if auth != 0 {
			p.fault(call, 0, rpcProtocolError)
		} else {
			switch h[2] {
			case rpcBind, rpcAlterContext:
				p.bind(h[:n])
			case rpcRequest:
				p.request(h[:n])
			default:
				p.fault(call, 0, rpcProtocolError)
			}
		}
		p.input = p.input[n:]
		if len(p.output) > 16<<20 {
			p.input = nil
			return statusInsufficientResources
		}
	}
	if len(p.input) == 0 {
		p.input = nil
	}
	return statusSuccess
}

// read returns a bounded portion of the response byte stream, retaining unread bytes.
func (p *pipe) read(max int) ([]byte, uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readLocked(max)
}

// readLocked reports partial pipe reads using SMB's buffer-overflow warning.
func (p *pipe) readLocked(max int) ([]byte, uint32) {
	if max < 0 {
		return nil, statusInvalidParameter
	}
	if len(p.output) == 0 {
		return nil, statusPipeEmpty
	}
	n := len(p.output)
	st := uint32(statusSuccess)
	if n > max {
		n = max
		st = statusBufferOverflow
	}
	b := append([]byte(nil), p.output[:n]...)
	p.output = p.output[n:]
	if len(p.output) == 0 {
		p.output = nil
	}
	return b, st
}

// transact atomically submits an RPC request and reads its reply for PIPE_TRANSCEIVE.
func (p *pipe) transact(in []byte, maxOut int) ([]byte, uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.writeLocked(in); st != statusSuccess {
		return nil, st
	}
	return p.readLocked(maxOut)
}

// rpcPDU wraps an RPC body in a little-endian connection-oriented common header.
func rpcPDU(kind, flags byte, call uint32, body []byte) []byte {
	b := make([]byte, 16, 16+len(body))
	b[0], b[2], b[3], b[4] = 5, kind, flags, 0x10
	binary.LittleEndian.PutUint16(b[8:], uint16(16+len(body)))
	binary.LittleEndian.PutUint32(b[12:], call)
	return append(b, body...)
}

// bind negotiates SRVS v3 with NDR32, explicitly rejecting unsupported interfaces and NDR64.
func (p *pipe) bind(b []byte) {
	call := binary.LittleEndian.Uint32(b[12:])
	if len(b) < 28 || b[3]&(rpcFirst|rpcLast) != rpcFirst|rpcLast {
		p.fault(call, 0, rpcProtocolError)
		return
	}
	if p.name != "srvsvc" {
		p.output = append(p.output, rpcPDU(rpcBindNak, rpcFirst|rpcLast, call, []byte{2, 0, 1, 5, 0})...)
		return
	}
	maxRecv := int(binary.LittleEndian.Uint16(b[18:]))
	if maxRecv < 128 {
		p.output = append(p.output, rpcPDU(rpcBindNak, rpcFirst|rpcLast, call, []byte{2, 0, 1, 5, 0})...)
		return
	}
	p.maxRecv = maxRecv
	count, off := int(b[24]), 28
	if count == 0 {
		p.fault(call, 0, rpcProtocolError)
		return
	}
	results := make([]byte, 4, 4+24*count)
	results[0] = byte(count)
	for range count {
		if off+24 > len(b) {
			p.fault(call, 0, rpcProtocolError)
			return
		}
		id, transfers := binary.LittleEndian.Uint16(b[off:]), int(b[off+2])
		if transfers == 0 || transfers > (len(b)-off-24)/20 {
			p.fault(call, 0, rpcProtocolError)
			return
		}
		abstract := string(b[off+4:off+24]) == string(srvsvcSyntax[:])
		accepted := false
		for j := range transfers {
			if string(b[off+24+j*20:off+44+j*20]) == string(ndr20Syntax[:]) {
				accepted = true
			}
		}
		result, reason := uint16(2), uint16(1)
		if abstract && accepted {
			result, reason = 0, 0
			p.contexts[id] = true
		} else {
			if abstract {
				reason = 2
			}
			delete(p.contexts, id)
		}
		results = binary.LittleEndian.AppendUint16(results, result)
		results = binary.LittleEndian.AppendUint16(results, reason)
		if result == 0 {
			results = append(results, ndr20Syntax[:]...)
		} else {
			results = append(results, make([]byte, 20)...)
		}
		off += 24 + 20*transfers
	}
	if off != len(b) {
		p.fault(call, 0, rpcProtocolError)
		return
	}
	body := binary.LittleEndian.AppendUint16(nil, uint16(p.maxRecv))
	body = binary.LittleEndian.AppendUint16(body, uint16(p.maxRecv))
	body = binary.LittleEndian.AppendUint32(body, 1)
	endpoint := "\\PIPE\\srvsvc\x00"
	kind := byte(rpcBindAck)
	if b[2] == rpcAlterContext {
		endpoint = ""
		kind = rpcAlterContextResponse
	}
	body = binary.LittleEndian.AppendUint16(body, uint16(len(endpoint)))
	body = append(body, endpoint...)
	for len(body)&3 != 0 {
		body = append(body, 0)
	}
	body = append(body, results...)
	p.output = append(p.output, rpcPDU(kind, rpcFirst|rpcLast, call, body)...)
}

// request reassembles call fragments and dispatches the supported SRVS operations.
func (p *pipe) request(b []byte) {
	call := binary.LittleEndian.Uint32(b[12:])
	if len(b) < 24 || b[3]&0x80 != 0 {
		p.fault(call, 0, rpcProtocolError)
		return
	}
	context, op := binary.LittleEndian.Uint16(b[20:]), binary.LittleEndian.Uint16(b[22:])
	if !p.contexts[context] {
		p.fault(call, context, rpcUnknownInterface)
		return
	}
	c := p.calls[call]
	if b[3]&rpcFirst != 0 {
		if c != nil || len(p.calls) >= 128 {
			p.fault(call, context, rpcProtocolError)
			return
		}
		c = &rpcCall{context: context, op: op}
		p.calls[call] = c
	}
	if c == nil || c.context != context || c.op != op {
		if c != nil {
			p.callBytes -= len(c.stub)
		}
		delete(p.calls, call)
		p.fault(call, context, rpcProtocolError)
		return
	}
	if p.callBytes+len(b)-24 > 16<<20 {
		p.callBytes -= len(c.stub)
		delete(p.calls, call)
		p.fault(call, context, rpcBadStub)
		return
	}
	c.stub = append(c.stub, b[24:]...)
	p.callBytes += len(b) - 24
	if b[3]&rpcLast == 0 {
		return
	}
	delete(p.calls, call)
	p.callBytes -= len(c.stub)
	var out []byte
	var ok bool
	switch op {
	case 15:
		out, ok = p.shareEnum(c.stub)
	case 16:
		out, ok = p.shareInfo(c.stub)
	case 21:
		out, ok = p.serverInfo(c.stub)
	default:
		p.fault(call, context, rpcOpRangeError)
		return
	}
	if !ok {
		p.fault(call, context, rpcBadStub)
		return
	}
	p.respond(call, context, out)
}

// fault returns a terminal connection-oriented RPC fault rather than an SMB transport error.
func (p *pipe) fault(call uint32, context uint16, code uint32) {
	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[4:], context)
	binary.LittleEndian.PutUint32(body[8:], code)
	p.output = append(p.output, rpcPDU(rpcFault, rpcFirst|rpcLast, call, body)...)
}

// respond fragments a response at the peer's advertised maximum receive size.
func (p *pipe) respond(call uint32, context uint16, stub []byte) {
	limit := p.maxRecv - 24
	if limit < 1 {
		limit = 4256
	}
	first := true
	for first || len(stub) != 0 {
		n := min(len(stub), limit)
		body := make([]byte, 8, 8+n)
		binary.LittleEndian.PutUint32(body, uint32(len(stub)))
		binary.LittleEndian.PutUint16(body[4:], context)
		body = append(body, stub[:n]...)
		flags := byte(0)
		if first {
			flags |= rpcFirst
		}
		if n == len(stub) {
			flags |= rpcLast
		}
		p.output = append(p.output, rpcPDU(rpcResponse, flags, call, body)...)
		stub, first = stub[n:], false
	}
}

// ndrReader decodes the scalar and wide-string portions of SRVS request stubs.
type ndrReader struct {
	b     []byte
	off   int
	valid bool
}

// u32 reads an aligned little-endian NDR scalar and remembers malformed input.
func (n *ndrReader) u32() uint32 {
	n.off = (n.off + 3) &^ 3
	if !n.valid || n.off > len(n.b)-4 {
		n.valid = false
		return 0
	}
	v := binary.LittleEndian.Uint32(n.b[n.off:])
	n.off += 4
	return v
}

// wide reads a conformant varying UTF-16 string, including its terminating NUL.
func (n *ndrReader) wide() string {
	maximum, offset, actual := n.u32(), n.u32(), n.u32()
	if !n.valid || actual == 0 || offset > maximum || actual > maximum-offset || uint64(actual)*2 > uint64(len(n.b)-n.off) {
		n.valid = false
		return ""
	}
	b := n.b[n.off : n.off+int(actual)*2]
	n.off += len(b)
	if b[len(b)-2] != 0 || b[len(b)-1] != 0 {
		n.valid = false
		return ""
	}
	return utf16Decode(b[:len(b)-2])
}

// serverName consumes the optional SRVS server-name parameter.
func (n *ndrReader) serverName() {
	if n.u32() != 0 {
		_ = n.wide()
	}
}

// ndrWriter builds deterministic NDR20 scalar fields and deferred referents.
type ndrWriter struct {
	b    []byte
	next uint32
}

// u32 appends an aligned NDR scalar.
func (n *ndrWriter) u32(v uint32) {
	for len(n.b)&3 != 0 {
		n.b = append(n.b, 0)
	}
	n.b = binary.LittleEndian.AppendUint32(n.b, v)
}

// pointer appends a non-null unique-pointer referent identifier.
func (n *ndrWriter) pointer() {
	if n.next == 0 {
		n.next = 0x20000
	}
	n.u32(n.next)
	n.next += 4
}

// wide appends the conformant varying array behind an NDR wide-string pointer.
func (n *ndrWriter) wide(s string) {
	count := uint32(utf16Len(s)/2 + 1)
	n.u32(count)
	n.u32(0)
	n.u32(count)
	n.b = utf16Append(n.b, s)
	n.b = append(n.b, 0, 0)
	for len(n.b)&3 != 0 {
		n.b = append(n.b, 0)
	}
}

// shareName returns the exported disk share, including the default for direct Server users.
func (p *pipe) shareName() string {
	if p.srv != nil && p.srv.Share != "" {
		return p.srv.Share
	}
	return "ovnfs"
}

// shareEnum implements NetrShareEnum levels zero and one, including continuation handles.
func (p *pipe) shareEnum(b []byte) ([]byte, bool) {
	r := ndrReader{b: b, valid: true}
	r.serverName()
	level, arm := r.u32(), r.u32()
	container := r.u32()
	if !r.valid || arm != level {
		return nil, false
	}
	if level != 0 && level != 1 {
		w := ndrWriter{}
		w.u32(level)
		w.u32(level)
		w.u32(0)
		w.u32(0)
		w.u32(0)
		w.u32(124)
		return w.b, true
	}
	if container != 0 {
		if r.u32() != 0 || r.u32() != 0 {
			return nil, false
		}
	}
	preferred := r.u32()
	resumePtr := r.u32()
	resume := uint32(0)
	if resumePtr != 0 {
		resume = r.u32()
	}
	if !r.valid || resume > 2 {
		return nil, false
	}
	names := []string{p.shareName(), "IPC$"}
	types := []uint32{0, 0x80000003}
	remarks := []string{"ovnfs overlay filesystem", "Remote IPC"}
	end := uint32(2)
	// A bounded preferred length is a pagination hint; never split an NDR entry.
	if preferred != 0xffffffff {
		used := uint64(32)
		end = resume
		for end < 2 {
			size := uint64(16 + utf16Len(names[end]) + 2)
			if level == 1 {
				size += uint64(20 + utf16Len(remarks[end]) + 2)
			}
			if used+size > uint64(preferred) {
				break
			}
			used += size
			end++
		}
	}
	w := ndrWriter{}
	w.u32(level)
	w.u32(level)
	w.pointer()
	w.u32(end - resume)
	if end == resume {
		w.u32(0)
	} else {
		w.pointer()
		w.u32(end - resume)
		for i := resume; i < end; i++ {
			w.pointer()
			if level == 1 {
				w.u32(types[i])
				w.pointer()
			}
		}
		for i := resume; i < end; i++ {
			w.wide(names[i])
			if level == 1 {
				w.wide(remarks[i])
			}
		}
	}
	w.u32(2)
	if resumePtr != 0 {
		w.pointer()
		if end < 2 {
			w.u32(end)
		} else {
			w.u32(0)
		}
	} else {
		w.u32(0)
	}
	if end < 2 {
		w.u32(234)
	} else {
		w.u32(0)
	}
	return w.b, true
}

// shareInfo returns supported share-information union arms for the disk share and IPC$.
func (p *pipe) shareInfo(b []byte) ([]byte, bool) {
	r := ndrReader{b: b, valid: true}
	r.serverName()
	name := r.wide()
	level := r.u32()
	if !r.valid {
		return nil, false
	}
	w := ndrWriter{}
	w.u32(level)
	ipc := strings.EqualFold(name, "IPC$")
	if !ipc && !strings.EqualFold(name, p.shareName()) {
		w.u32(0)
		w.u32(2310)
		return w.b, true
	}
	if level != 0 && level != 1 && level != 2 && level != 501 && level != 1005 {
		w.u32(0)
		w.u32(124)
		return w.b, true
	}
	w.pointer()
	if level == 1005 {
		w.u32(0)
		w.u32(0)
		return w.b, true
	}
	typ, remark := uint32(0), "ovnfs overlay filesystem"
	if ipc {
		typ, remark = 0x80000003, "Remote IPC"
		name = "IPC$"
	} else {
		name = p.shareName()
	}
	w.pointer()
	if level != 0 {
		w.u32(typ)
		w.pointer()
	}
	if level == 2 {
		w.u32(0)
		w.u32(0xffffffff)
		w.u32(0)
		w.pointer()
		w.u32(0)
	}
	if level == 501 {
		w.u32(0)
	}
	w.wide(name)
	if level != 0 {
		w.wide(remark)
	}
	if level == 2 {
		w.wide("C:\\" + name)
	}
	w.u32(0)
	return w.b, true
}

// serverInfo returns the SRVS level-101 identity used by Windows and macOS browsers.
func (p *pipe) serverInfo(b []byte) ([]byte, bool) {
	r := ndrReader{b: b, valid: true}
	r.serverName()
	level := r.u32()
	if !r.valid {
		return nil, false
	}
	w := ndrWriter{}
	w.u32(level)
	if level != 101 {
		w.u32(0)
		w.u32(124)
		return w.b, true
	}
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "OVNFS"
	}
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		name = name[:dot]
	}
	name = strings.ToUpper(name)
	w.pointer()
	w.u32(500)
	w.pointer()
	w.u32(10)
	w.u32(0)
	w.u32(0x00001003)
	w.pointer()
	w.wide(name)
	w.wide("ovnfs overlay filesystem")
	w.u32(0)
	return w.b, true
}
