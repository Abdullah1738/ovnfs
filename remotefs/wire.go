package remotefs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
)

// Kind is a v1 node kind: anything that is not a directory is a file.
type Kind string

// Node kinds of the v1 wire.
const (
	// KindFile is every non-directory node.
	KindFile Kind = "file"
	// KindDir is a directory.
	KindDir Kind = "dir"
)

// Stat is the reply of stat, create, mkdir and setattr (vibevmm proto::StatReply).
type Stat struct {
	// Kind is the node kind.
	Kind Kind `json:"kind"`
	// Size is the byte length, zero for directories.
	Size uint64 `json:"size"`
	// Mtime is the modification time in whole Unix seconds.
	Mtime uint64 `json:"mtime"`
	// Etag is always null: checkouts are not object stores.
	Etag *string `json:"etag"`
	// Mode holds the permission bits (mode & 0o7777); nil means the device default.
	Mode *uint32 `json:"mode,omitempty"`
	// UID is the owner as the guest sees it; nil means 0.
	UID *uint32 `json:"uid,omitempty"`
	// GID is the group as the guest sees it; nil means 0.
	GID *uint32 `json:"gid,omitempty"`
}

// Entry is one child in a list reply (vibevmm proto::Entry).
type Entry struct {
	// Name is one path segment.
	Name string `json:"name"`
	// Kind is the child's kind.
	Kind Kind `json:"kind"`
	// Size is the byte length, zero for directories.
	Size uint64 `json:"size"`
	// Mtime is the modification time in whole Unix seconds.
	Mtime uint64 `json:"mtime"`
	// Mode holds the permission bits when known.
	Mode *uint32 `json:"mode,omitempty"`
	// UID is the owner when known.
	UID *uint32 `json:"uid,omitempty"`
	// GID is the group when known.
	GID *uint32 `json:"gid,omitempty"`
}

// listReply is the reply of list: children sorted by name.
type listReply struct {
	Entries []Entry `json:"entries"`
}

// writeReply is the reply of write.
type writeReply struct {
	Written uint32 `json:"written"`
}

// emptyReply is the OK_JSON body of mutations without a result.
var emptyReply = []byte("{}")

// Error codes of the v1 wire; vibevmm maps each to the errno in its comment, anything else to EIO.
const (
	CodeNotFound    = "not_found"   // ENOENT
	CodeAccess      = "access"      // EACCES
	CodeBadRequest  = "bad_request" // EINVAL
	CodeStale       = "stale"       // EIO (no mapping in the VMM)
	CodeIO          = "io"          // EIO
	CodeExists      = "exists"      // EEXIST
	CodeNotEmpty    = "not_empty"   // ENOTEMPTY
	CodeIsDir       = "is_dir"      // EISDIR
	CodeNotDir      = "not_dir"     // ENOTDIR
	CodeXdev        = "xdev"        // EXDEV
	CodePerm        = "perm"        // EPERM
	CodeRofs        = "rofs"        // EROFS
	CodeTooBig      = "too_big"     // EFBIG
	CodeNoSpace     = "no_space"    // ENOSPC
	CodeUnsupported = "unsupported" // EOPNOTSUPP
)

// Error is a remote-fs failure as carried by an ERR frame (vibevmm proto::ErrReply).
type Error struct {
	// Code is the stable wire code, one of the Code* constants.
	Code string `json:"code"`
	// Msg is a human-readable diagnostic.
	Msg string `json:"msg"`
}

// Error formats the code and diagnostic.
func (e *Error) Error() string { return fmt.Sprintf("remotefs %s: %s", e.Code, e.Msg) }

// Is matches the io/fs sentinel errors that correspond to the code.
func (e *Error) Is(target error) bool {
	switch target {
	case fs.ErrNotExist:
		return e.Code == CodeNotFound
	case fs.ErrExist:
		return e.Code == CodeExists
	case fs.ErrPermission:
		return e.Code == CodePerm || e.Code == CodeAccess
	case fs.ErrInvalid:
		return e.Code == CodeBadRequest
	}
	return false
}

// Request ops of the v1 wire.
const (
	opStat    = "stat"
	opList    = "list"
	opRead    = "read"
	opCreate  = "create"
	opMkdir   = "mkdir"
	opWrite   = "write"
	opSetattr = "setattr"
	opSync    = "sync"
	opUnlink  = "unlink"
	opRmdir   = "rmdir"
	opRename  = "rename"
	opHello   = "hello"
)

// request is a decoded v1 request: every op's fields, nil when absent.
type request struct {
	Op        string  `json:"op"`
	Path      *string `json:"path"`
	From      *string `json:"from"`
	To        *string `json:"to"`
	Offset    *uint64 `json:"offset"`
	Len       *uint32 `json:"len"`
	Mode      *uint32 `json:"mode"`
	UID       *uint32 `json:"uid"`
	GID       *uint32 `json:"gid"`
	Excl      *bool   `json:"excl"`
	Trunc     *bool   `json:"trunc"`
	Size      *uint64 `json:"size"`
	Mtime     *uint64 `json:"mtime"`
	Release   *bool   `json:"release"`
	Noreplace *bool   `json:"noreplace"`
}

// missing names the first field op requires that r lacks, or "" when complete; tag is ignored.
func (r *request) missing() string {
	need := func(fields ...struct {
		name string
		ok   bool
	}) string {
		for _, f := range fields {
			if !f.ok {
				return f.name
			}
		}
		return ""
	}
	type f = struct {
		name string
		ok   bool
	}
	path := f{"path", r.Path != nil}
	switch r.Op {
	case opStat, opList, opUnlink, opRmdir, opSetattr:
		return need(path)
	case opRead:
		return need(path, f{"offset", r.Offset != nil}, f{"len", r.Len != nil})
	case opCreate:
		return need(path, f{"mode", r.Mode != nil}, f{"uid", r.UID != nil}, f{"gid", r.GID != nil}, f{"excl", r.Excl != nil}, f{"trunc", r.Trunc != nil})
	case opMkdir:
		return need(path, f{"mode", r.Mode != nil}, f{"uid", r.UID != nil}, f{"gid", r.GID != nil})
	case opWrite:
		return need(path, f{"offset", r.Offset != nil})
	case opSync:
		return need(path, f{"release", r.Release != nil})
	case opRename:
		return need(f{"from", r.From != nil}, f{"to", r.To != nil}, f{"noreplace", r.Noreplace != nil})
	}
	return ""
}

// Client-side request shapes, field for field in vibevmm's serde order so frames are byte-identical.
type (
	// pathReq encodes stat, list, unlink and rmdir.
	pathReq struct {
		Op   string `json:"op"`
		Tag  string `json:"tag"`
		Path string `json:"path"`
	}
	// readReq encodes read.
	readReq struct {
		Op     string `json:"op"`
		Tag    string `json:"tag"`
		Path   string `json:"path"`
		Offset uint64 `json:"offset"`
		Len    uint32 `json:"len"`
	}
	// createReq encodes create.
	createReq struct {
		Op    string `json:"op"`
		Tag   string `json:"tag"`
		Path  string `json:"path"`
		Mode  uint32 `json:"mode"`
		UID   uint32 `json:"uid"`
		GID   uint32 `json:"gid"`
		Excl  bool   `json:"excl"`
		Trunc bool   `json:"trunc"`
	}
	// mkdirReq encodes mkdir.
	mkdirReq struct {
		Op   string `json:"op"`
		Tag  string `json:"tag"`
		Path string `json:"path"`
		Mode uint32 `json:"mode"`
		UID  uint32 `json:"uid"`
		GID  uint32 `json:"gid"`
	}
	// writeReq encodes write; its data follows in the REQ_DATA frame.
	writeReq struct {
		Op     string `json:"op"`
		Tag    string `json:"tag"`
		Path   string `json:"path"`
		Offset uint64 `json:"offset"`
	}
	// setattrReq encodes setattr; absent fields stay unchanged.
	setattrReq struct {
		Op    string  `json:"op"`
		Tag   string  `json:"tag"`
		Path  string  `json:"path"`
		Size  *uint64 `json:"size,omitempty"`
		Mode  *uint32 `json:"mode,omitempty"`
		UID   *uint32 `json:"uid,omitempty"`
		GID   *uint32 `json:"gid,omitempty"`
		Mtime *uint64 `json:"mtime,omitempty"`
	}
	// syncReq encodes sync.
	syncReq struct {
		Op      string `json:"op"`
		Tag     string `json:"tag"`
		Path    string `json:"path"`
		Release bool   `json:"release"`
	}
	// renameReq encodes rename.
	renameReq struct {
		Op        string `json:"op"`
		Tag       string `json:"tag"`
		From      string `json:"from"`
		To        string `json:"to"`
		Noreplace bool   `json:"noreplace"`
	}
)

// encoder marshals JSON like serde_json: no HTML escaping and no trailing newline.
type encoder struct {
	buf bytes.Buffer
	enc *json.Encoder
}

// newEncoder returns an encoder with a reusable buffer.
func newEncoder() *encoder {
	e := &encoder{}
	e.enc = json.NewEncoder(&e.buf)
	e.enc.SetEscapeHTML(false)
	return e
}

// marshal encodes v; the result is valid until the next call.
func (e *encoder) marshal(v any) ([]byte, error) {
	e.buf.Reset()
	if err := e.enc.Encode(v); err != nil {
		return nil, err
	}
	b := e.buf.Bytes()
	return b[:len(b)-1], nil
}
