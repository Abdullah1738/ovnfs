package remotefs

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sync"
)

// Client speaks remote-fs v1 to one share socket, one request at a time,
// exactly as vibevmm's device does (but with a fresh id per request).
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	tag  string
	next uint32
	enc  *encoder
	out  []byte
	in   []byte
}

// Dial connects to the share socket at path; tag is sent in every request (servers ignore it).
func Dial(path, tag string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("remotefs: dial %s: %w", path, err)
	}
	return NewClient(conn, tag), nil
}

// NewClient speaks remote-fs v1 over an established connection.
func NewClient(conn net.Conn, tag string) *Client {
	return &Client{conn: conn, r: bufio.NewReaderSize(conn, 64<<10), tag: tag, enc: newEncoder()}
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// SetAttr lists the attributes Setattr changes; nil fields stay unchanged.
type SetAttr struct {
	// Size truncates or extends the file.
	Size *uint64
	// Mode sets the permission bits.
	Mode *uint32
	// UID sets the owner.
	UID *uint32
	// GID sets the group.
	GID *uint32
	// Mtime sets the modification time in Unix seconds.
	Mtime *uint64
}

// Stat describes path ("" is the checkout root), following symlinks inside the checkout.
func (c *Client) Stat(path string) (Stat, error) {
	return c.statCall(pathReq{Op: opStat, Tag: c.tag, Path: path})
}

// List returns the children of a directory sorted by name.
func (c *Client) List(path string) ([]Entry, error) {
	var out listReply
	err := c.jsonCall(pathReq{Op: opList, Tag: c.tag, Path: path}, nil, &out)
	return out.Entries, err
}

// Read returns up to n bytes of path at off; fewer at end of file.
func (c *Client) Read(path string, off uint64, n uint32) ([]byte, error) {
	var out []byte
	err := c.call(readReq{Op: opRead, Tag: c.tag, Path: path, Offset: off, Len: n}, nil, func(ty byte, body []byte) error {
		if ty != FrameOKData {
			return fmt.Errorf("remotefs: read answered with frame type %d", ty)
		}
		out = append([]byte{}, body...)
		return nil
	})
	return out, err
}

// Create creates the regular file path, or opens it unless excl; trunc empties an existing file.
// uid and gid are the caller's ids as the VM sees them.
func (c *Client) Create(path string, mode, uid, gid uint32, excl, trunc bool) (Stat, error) {
	return c.statCall(createReq{Op: opCreate, Tag: c.tag, Path: path, Mode: mode, UID: uid, GID: gid, Excl: excl, Trunc: trunc})
}

// Mkdir creates the directory path.
func (c *Client) Mkdir(path string, mode, uid, gid uint32) (Stat, error) {
	return c.statCall(mkdirReq{Op: opMkdir, Tag: c.tag, Path: path, Mode: mode, UID: uid, GID: gid})
}

// Write stores data in path at off and returns the bytes accepted.
func (c *Client) Write(path string, off uint64, data []byte) (uint32, error) {
	if data == nil {
		data = []byte{}
	}
	var out writeReply
	err := c.jsonCall(writeReq{Op: opWrite, Tag: c.tag, Path: path, Offset: off}, data, &out)
	return out.Written, err
}

// Setattr changes the attributes set in a and returns the new ones.
func (c *Client) Setattr(path string, a SetAttr) (Stat, error) {
	return c.statCall(setattrReq{Op: opSetattr, Tag: c.tag, Path: path, Size: a.Size, Mode: a.Mode, UID: a.UID, GID: a.GID, Mtime: a.Mtime})
}

// Sync persists path's written data; release tells the server a guest handle closed.
func (c *Client) Sync(path string, release bool) error {
	return c.jsonCall(syncReq{Op: opSync, Tag: c.tag, Path: path, Release: release}, nil, nil)
}

// Unlink removes a non-directory.
func (c *Client) Unlink(path string) error {
	return c.jsonCall(pathReq{Op: opUnlink, Tag: c.tag, Path: path}, nil, nil)
}

// Rmdir removes an empty directory.
func (c *Client) Rmdir(path string) error {
	return c.jsonCall(pathReq{Op: opRmdir, Tag: c.tag, Path: path}, nil, nil)
}

// Rename moves from to to; noreplace fails when to exists.
func (c *Client) Rename(from, to string, noreplace bool) error {
	return c.jsonCall(renameReq{Op: opRename, Tag: c.tag, From: from, To: to, Noreplace: noreplace}, nil, nil)
}

// statCall sends a request that replies with a Stat.
func (c *Client) statCall(req any) (Stat, error) {
	var st Stat
	err := c.jsonCall(req, nil, &st)
	return st, err
}

// jsonCall sends a request and decodes its OK_JSON reply into out (nil ignores it).
func (c *Client) jsonCall(req any, data []byte, out any) error {
	return c.call(req, data, func(ty byte, body []byte) error {
		if ty != FrameOKJSON {
			return fmt.Errorf("remotefs: expected a JSON reply, got frame type %d", ty)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("remotefs: decode reply: %w", err)
		}
		return nil
	})
}

// call sends one request (REQ_DATA when data is non-nil) and hands the reply
// frame to handle while the connection is still held; an ERR frame becomes an *Error.
func (c *Client) call(req any, data []byte, handle func(ty byte, body []byte) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, err := c.enc.marshal(req)
	if err != nil {
		return fmt.Errorf("remotefs: encode request: %w", err)
	}
	c.next++
	id := c.next
	if data == nil {
		c.out, err = appendFrame(c.out[:0], FrameReq, id, body)
	} else {
		c.out, err = appendFrame(c.out[:0], FrameReqData, id, binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body, data)
	}
	if err != nil {
		return err
	}
	if _, err := c.conn.Write(c.out); err != nil {
		return fmt.Errorf("remotefs: send request: %w", err)
	}
	ty, rid, payload, err := readFrame(c.r, c.in)
	c.in = payload[:0]
	if err != nil {
		return fmt.Errorf("remotefs: read reply: %w", err)
	}
	if rid != id {
		return fmt.Errorf("remotefs: reply id %d for request %d", rid, id)
	}
	if ty == FrameErr {
		e := &Error{}
		if err := json.Unmarshal(payload, e); err != nil {
			return fmt.Errorf("remotefs: decode error reply: %w", err)
		}
		return e
	}
	return handle(ty, payload)
}
