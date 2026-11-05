package nfs

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The NFSv4.0 callback program (RFC 7530 section 16.33): the server calls
// the client back, over a TCP connection it opens to the address the client
// gave in SETCLIENTID, to recall delegations. This is a minimal ONC RPC
// client for it: CB_NULL, and CB_COMPOUND carrying one CB_RECALL.

// Callback program values and limits.
const (
	cb4Version   = 1
	cb4Null      = 0
	cb4Compound  = 1
	cb4OpRecall  = 4
	cb4DialLimit = 5 * time.Second
	cb4CallLimit = 15 * time.Second
)

// v4Callback is a client's callback path: program, address and
// callback_ident from SETCLIENTID, and the connection calls go over, opened
// on first use and again after a failure.
type v4Callback struct {
	prog  uint32
	addr  string // host:port
	ident uint32

	// calls serializes calls; xid and r belong to the call in progress.
	calls sync.Mutex
	xid   uint32
	r     *bufio.Reader

	mu     sync.Mutex
	nc     net.Conn
	closed bool
}

// newCallback4 reads a SETCLIENTID cb_client4. A client giving no usable
// address (the macOS client mounted with nocallback sends empty strings)
// has no callback path, which only means it gets no delegations.
func newCallback4(prog uint32, netid, uaddr string, ident uint32) (*v4Callback, error) {
	addr, err := parseUaddr4(netid, uaddr)
	if err != nil {
		return nil, err
	}
	return &v4Callback{prog: prog, addr: addr, ident: ident}, nil
}

// parseUaddr4 turns an RFC 5665 universal address, the host followed by the
// port's two bytes as ".p1.p2", into host:port.
func parseUaddr4(netid, uaddr string) (string, error) {
	if netid != "tcp" && netid != "tcp6" {
		return "", fmt.Errorf("unsupported netid %q", netid)
	}
	lo := strings.LastIndexByte(uaddr, '.')
	if lo < 0 {
		return "", errors.New("no port")
	}
	hi := strings.LastIndexByte(uaddr[:lo], '.')
	if hi < 0 {
		return "", errors.New("no port")
	}
	p1, err1 := strconv.ParseUint(uaddr[hi+1:lo], 10, 8)
	p2, err2 := strconv.ParseUint(uaddr[lo+1:], 10, 8)
	host := net.ParseIP(uaddr[:hi])
	if err1 != nil || err2 != nil || host == nil {
		return "", fmt.Errorf("malformed address %q", uaddr)
	}
	port := p1<<8 | p2
	if port == 0 {
		return "", errors.New("port 0")
	}
	return net.JoinHostPort(host.String(), strconv.FormatUint(port, 10)), nil
}

// close ends the callback path: a call in progress fails, and later ones
// fail at once. It does not block.
func (cb *v4Callback) close() {
	if cb == nil {
		return
	}
	cb.mu.Lock()
	cb.closed = true
	nc := cb.nc
	cb.nc = nil
	cb.mu.Unlock()
	if nc != nil {
		_ = nc.Close()
	}
}

// null probes the path with CB_NULL.
func (cb *v4Callback) null() error {
	_, err := cb.call(cb4Null, nil, nil)
	return err
}

// errCBSkipped reports a call its precondition dropped.
var errCBSkipped = errors.New("callback no longer wanted")

// recall sends CB_RECALL {stateid, truncate false, fh} of a delegation of
// file in a CB_COMPOUND, if want, asked once the call's turn has come,
// still wants it (errCBSkipped if not). A transport failure is an error;
// the client's answer is the status.
func (cb *v4Callback) recall(id v4StateID, file uint64, want func() bool) (status, error) {
	body, err := cb.call(cb4Compound, want, func(e *encoder) {
		e.str("recall")
		e.u32(0) // minorversion
		e.u32(cb.ident)
		e.u32(1)
		e.u32(cb4OpRecall)
		putStateID4(e, id)
		e.bool(false)
		e.fh(file)
	})
	if err != nil {
		return 0, err
	}
	d := decoder{buf: body}
	s := status(d.u32())
	if d.err != nil {
		return 0, errors.New("short CB_COMPOUND reply")
	}
	return s, nil
}

// call makes one ONC RPC call, once want (if not nil) agrees when the
// call's turn has come, and returns the reply's results. Calls go one at a
// time over one connection, kept between calls.
//
// Clients close idle callback connections (macOS after 300 s, Linux after
// about 6 minutes), which the server learns only when a call on one fails.
// A call that fails on a reused connection before any reply came is sent
// again, once, on a new connection: CB_NULL and CB_RECALL are safe to
// repeat. A timeout is not retried, as the client may be busy with the
// call.
func (cb *v4Callback) call(proc uint32, want func() bool, args func(*encoder)) ([]byte, error) {
	cb.calls.Lock()
	defer cb.calls.Unlock()
	if want != nil && !want() {
		return nil, errCBSkipped
	}
	for retry := true; ; retry = false {
		cb.mu.Lock()
		closed, nc := cb.closed, cb.nc
		cb.mu.Unlock()
		if closed {
			return nil, errors.New("callback path closed")
		}
		reused := nc != nil
		if !reused {
			var err error
			if nc, err = net.DialTimeout("tcp", cb.addr, cb4DialLimit); err != nil {
				return nil, err
			}
			cb.mu.Lock()
			if cb.closed {
				cb.mu.Unlock()
				nc.Close()
				return nil, errors.New("callback path closed")
			}
			cb.nc = nc
			cb.mu.Unlock()
			cb.r = bufio.NewReader(nc)
		}
		body, replied, err := cb.roundTrip(nc, proc, args)
		if err == nil {
			return body, nil
		}
		cb.mu.Lock()
		if cb.nc == nc {
			cb.nc = nil
		}
		cb.mu.Unlock()
		nc.Close()
		var ne net.Error
		if !retry || !reused || replied || errors.As(err, &ne) && ne.Timeout() {
			return nil, err
		}
	}
}

// cb4Machine is the machine name of the callbacks' AUTH_SYS credential.
var cb4Machine = sync.OnceValue(func() string {
	name, err := os.Hostname()
	if err != nil || name == "" || len(name) > 255 {
		return "localhost"
	}
	return name
})

// roundTrip sends one call over nc and reads its reply, skipping late
// replies to calls that timed out; replied reports whether any reply came.
//
// The credential is AUTH_SYS as root: Linux's callback service refuses
// AUTH_NULL for every procedure but CB_NULL, and macOS's ignores it.
func (cb *v4Callback) roundTrip(nc net.Conn, proc uint32, args func(*encoder)) (body []byte, replied bool, err error) {
	cb.xid++
	xid := cb.xid
	e := encoder{}
	e.u32(0) // record mark
	e.u32(xid)
	e.u32(msgCall)
	e.u32(2) // RPC version
	e.u32(cb.prog)
	e.u32(cb4Version)
	e.u32(proc)
	e.u32(authSys)
	cred := e.len()
	e.u32(0)
	e.u32(0) // stamp
	e.str(cb4Machine())
	e.u32(0) // uid
	e.u32(0) // gid
	e.u32(0) // no supplementary groups
	e.put32(cred, uint32(e.len()-cred-4))
	e.u32(authNone)
	e.u32(0)
	if args != nil {
		args(&e)
	}
	e.put32(0, lastFragment|uint32(e.len()-4))
	if err := nc.SetDeadline(time.Now().Add(cb4CallLimit)); err != nil {
		return nil, false, err
	}
	if _, err := nc.Write(e.buf); err != nil {
		return nil, false, err
	}
	for {
		rec, err := readRecord(cb.r, nil)
		if err != nil {
			return nil, replied, err
		}
		replied = true
		d := decoder{buf: rec}
		rxid, mtype, rstat := d.u32(), d.u32(), d.u32()
		d.u32() // verifier
		d.opaque(maxAuthBytes)
		astat := acceptStat(d.u32())
		if d.err != nil {
			return nil, true, errors.New("malformed callback reply")
		}
		if rxid != xid {
			continue
		}
		if mtype != msgReply || rstat != replyAccepted || astat != rpcSuccess {
			return nil, true, fmt.Errorf("callback rejected: reply %d status %d/%d", mtype, rstat, astat)
		}
		return d.buf, true, nil
	}
}
