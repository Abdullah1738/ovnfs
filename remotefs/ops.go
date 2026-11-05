package remotefs

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-billy/v5"

	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// run executes one decoded request.
func (c *conn) run(req *request, data []byte) (reply, error) {
	switch req.Op {
	case opStat:
		if !validPath(*req.Path) {
			return reply{}, errorf(CodeBadRequest, fmt.Sprintf("invalid path %q", *req.Path))
		}
		info, err := c.describe(*req.Path)
		if err != nil {
			return reply{}, err
		}
		return c.ok(c.stat(info))
	case opList:
		p, err := c.resolve(*req.Path, false, true)
		if err != nil {
			return reply{}, err
		}
		entries, err := c.list(p)
		if err != nil {
			return reply{}, err
		}
		return c.ok(listReply{Entries: entries})
	case opRead:
		p, err := c.resolve(*req.Path, false, true)
		if err != nil {
			return reply{}, err
		}
		b, err := c.read(p, *req.Offset, *req.Len)
		if err != nil {
			return reply{}, err
		}
		return reply{ty: FrameOKData, body: b}, nil
	case opSync:
		p, err := c.resolve(*req.Path, false, true)
		if err != nil {
			return reply{}, err
		}
		if err := c.sync(p); err != nil {
			return reply{}, err
		}
		return reply{ty: FrameOKJSON, body: emptyReply}, nil
	case opCreate, opMkdir, opWrite, opSetattr, opUnlink, opRmdir, opRename:
		if !c.share.Writable {
			return reply{}, errorf(CodeRofs, "read-only share: "+req.Op)
		}
		return c.mutate(req, data)
	}
	return reply{}, errorf(CodeBadRequest, fmt.Sprintf("unknown op %q", req.Op))
}

// mutate executes one request that changes the checkout.
func (c *conn) mutate(req *request, data []byte) (reply, error) {
	if req.Op == opRename {
		from, err := c.resolve(*req.From, true, false)
		if err != nil {
			return reply{}, err
		}
		to, err := c.resolve(*req.To, true, false)
		if err != nil {
			return reply{}, err
		}
		if *req.Noreplace {
			if _, err := c.share.FS.Lstat(to); err == nil {
				return reply{}, errorf(CodeExists, "rename target exists: "+*req.To)
			}
		}
		if err := c.share.FS.Rename(from, to); err != nil {
			return reply{}, err
		}
		return reply{ty: FrameOKJSON, body: emptyReply}, nil
	}
	// Mutations of a name (mkdir, unlink, rmdir) act on a final symlink itself;
	// create, write and setattr act on what it points to.
	child := req.Op != opSetattr && req.Op != opWrite
	final := req.Op == opCreate || req.Op == opWrite || req.Op == opSetattr
	p, err := c.resolve(*req.Path, child, final)
	if err != nil {
		return reply{}, err
	}
	switch req.Op {
	case opCreate:
		err = c.create(p, *req.Mode, *req.UID, *req.GID, *req.Excl, *req.Trunc)
	case opMkdir:
		err = c.mkdir(p, *req.Mode, *req.UID, *req.GID)
	case opWrite:
		var n uint32
		if n, err = c.write(p, *req.Offset, data); err == nil {
			return c.ok(writeReply{Written: n})
		}
		return reply{}, err
	case opSetattr:
		err = c.setattr(p, req)
	case opUnlink, opRmdir:
		return reply{ty: FrameOKJSON, body: emptyReply}, c.remove(p, req.Op == opRmdir)
	}
	if err != nil {
		return reply{}, err
	}
	info, err := c.share.FS.Lstat(p)
	if err != nil {
		return reply{}, err
	}
	return c.ok(c.stat(info))
}

// maxSymlinks bounds the symlinks one path resolution follows (Linux's MAXSYMLINKS).
const maxSymlinks = 40

// resolve validates a wire path and returns its export path with symlinks
// resolved inside the checkout; child forbids the checkout root, and final also
// resolves a symlink in the last component.
func (c *conn) resolve(p string, child, final bool) (string, error) {
	if !validPath(p) {
		return "", errorf(CodeBadRequest, fmt.Sprintf("invalid path %q", p))
	}
	if p == "" && child {
		return "", errorf(CodeBadRequest, "operation needs a path below the checkout root")
	}
	return c.follow(p, final)
}

// join returns the export path of a checkout-relative path.
func (c *conn) join(rel string) string {
	if rel == "" {
		return c.share.Checkout
	}
	return c.share.Checkout + "/" + rel
}

// follow resolves the symlinks of checkout-relative rel, returning an export path.
// The VMM never sees symlinks, so the server resolves every component itself
// (the export follows only a final symlink); targets are confined to the checkout
// like a chroot, and absolute targets, which would name host paths, resolve to
// not_found. A missing final component is returned unresolved for creation.
func (c *conn) follow(rel string, final bool) (string, error) {
	if info, err := c.share.FS.Lstat(c.join(rel)); err == nil && (!final || info.Mode()&os.ModeSymlink == 0) {
		return c.join(rel), nil
	}
	pending := strings.Split(rel, "/")
	done := make([]string, 0, len(pending))
	for links := 0; len(pending) > 0; {
		seg := pending[0]
		pending = pending[1:]
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(done) > 0 {
				done = done[:len(done)-1]
			}
			continue
		}
		done = append(done, seg)
		last := len(pending) == 0
		if last && !final {
			break
		}
		at := c.join(strings.Join(done, "/"))
		info, err := c.share.FS.Lstat(at)
		if err != nil {
			if last && errors.Is(err, fs.ErrNotExist) {
				break
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if links++; links > maxSymlinks {
			return "", &fs.PathError{Op: "resolve", Path: rel, Err: syscall.ELOOP}
		}
		target, err := c.share.FS.Readlink(at)
		if err != nil {
			return "", err
		}
		if path.IsAbs(target) || filepath.IsAbs(target) {
			return "", errorf(CodeNotFound, fmt.Sprintf("symlink %s leaves the checkout: %s", at, target))
		}
		done = done[:len(done)-1]
		pending = append(strings.Split(filepath.ToSlash(target), "/"), pending...)
	}
	return c.join(strings.Join(done, "/")), nil
}

// rel returns the checkout-relative path of an export path inside the checkout.
func (c *conn) rel(p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(p, c.share.Checkout), "/")
}

// describe returns the attributes of checkout-relative rel through its symlinks;
// a dangling, looping or escaping final symlink is described as itself.
func (c *conn) describe(rel string) (os.FileInfo, error) {
	p, err := c.follow(rel, true)
	if err == nil {
		var info os.FileInfo
		if info, err = c.share.FS.Lstat(p); err == nil {
			return info, nil
		}
	}
	if lp, lerr := c.follow(rel, false); lerr == nil {
		if li, lerr := c.share.FS.Lstat(lp); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
			return li, nil
		}
	}
	return nil, err
}

// stat converts attributes to the wire shape with guest ownership.
func (c *conn) stat(info os.FileInfo) Stat {
	kind, size := KindFile, uint64(max(info.Size(), 0))
	if info.IsDir() {
		kind, size = KindDir, 0
	}
	mode := wireMode(info.Mode())
	st := Stat{Kind: kind, Size: size, Mtime: unixSeconds(info.ModTime()), Mode: &mode}
	if sys, ok := unixstat.Of(info); ok {
		uid, gid := toGuest(c.opts.UIDs, sys.UID), toGuest(c.opts.GIDs, sys.GID)
		st.UID, st.GID = &uid, &gid
	}
	return st
}

// list returns the children of p sorted by name; names the v1 wire cannot carry are skipped.
func (c *conn) list(p string) ([]Entry, error) {
	infos, err := c.share.FS.ReadDir(p)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(infos))
	for _, info := range infos {
		name := info.Name()
		if !validSegment(name) || !utf8.ValidString(name) {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := c.describe(c.rel(p + "/" + name)); err == nil {
				info = target
			}
		}
		st := c.stat(info)
		entries = append(entries, Entry{Name: name, Kind: st.Kind, Size: st.Size, Mtime: st.Mtime, Mode: st.Mode, UID: st.UID, GID: st.GID})
	}
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.Name, b.Name) })
	return entries, nil
}

// read returns up to n bytes of p at off, short at end of file.
func (c *conn) read(p string, off uint64, n uint32) ([]byte, error) {
	if n > MaxRead {
		return nil, errorf(CodeTooBig, fmt.Sprintf("read of %d bytes exceeds the %d-byte limit", n, MaxRead))
	}
	if off > math.MaxInt64 {
		return nil, errorf(CodeBadRequest, "read offset out of range")
	}
	f, err := c.share.FS.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if cap(c.data) < int(n) {
		c.data = make([]byte, n)
	}
	got, err := f.ReadAt(c.data[:n], int64(off))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return c.data[:got], nil
}

// create creates or opens the regular file p; a node it creates gets mode and the mapped owner.
func (c *conn) create(p string, mode, uid, gid uint32, excl, trunc bool) error {
	info, err := c.share.FS.Lstat(p)
	switch {
	case err == nil && info.IsDir():
		return errorf(CodeIsDir, "create over a directory: "+p)
	case err == nil && excl:
		return errorf(CodeExists, "create target exists: "+p)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	created := err != nil
	if created {
		if err := c.parentDir(p); err != nil {
			return err
		}
	}
	flag := os.O_CREATE | os.O_RDONLY
	if trunc {
		flag = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	if excl {
		flag |= os.O_EXCL
	}
	f, err := c.share.FS.OpenFile(p, flag, fileMode(mode))
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !created {
		return nil
	}
	return c.adopt(p, mode, uid, gid)
}

// mkdir creates the directory p, whose parent must exist, with mode and the mapped owner.
func (c *conn) mkdir(p string, mode, uid, gid uint32) error {
	if _, err := c.share.FS.Lstat(p); err == nil {
		return errorf(CodeExists, "mkdir target exists: "+p)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := c.parentDir(p); err != nil {
		return err
	}
	if err := c.share.FS.MkdirAll(p, fileMode(mode)); err != nil {
		return err
	}
	return c.adopt(p, mode, uid, gid)
}

// parentDir checks that p's parent exists and is a directory, so creation never
// conjures missing parents (billy's MkdirAll and some OpenFile implementations would).
func (c *conn) parentDir(p string) error {
	parent, err := c.share.FS.Lstat(path.Dir(p))
	if err != nil {
		return err
	}
	if !parent.IsDir() {
		return errorf(CodeNotDir, "parent is not a directory: "+path.Dir(p))
	}
	return nil
}

// adopt gives a node the server just created the requested mode (past the
// server's umask) and the caller's owner when the id maps cover it.
func (c *conn) adopt(p string, mode, uid, gid uint32) error {
	ch, ok := c.share.FS.(billy.Change)
	if !ok {
		return nil
	}
	if err := ch.Chmod(p, fileMode(mode)); err != nil {
		return err
	}
	hostUID, uidOK := mapped(c.opts.UIDs, uid)
	hostGID, gidOK := mapped(c.opts.GIDs, gid)
	if !uidOK && !gidOK {
		return nil
	}
	u, g := -1, -1
	if uidOK {
		u = int(hostUID)
	}
	if gidOK {
		g = int(hostGID)
	}
	return ch.Chown(p, u, g)
}

// write stores data in p at off and reports the bytes written.
func (c *conn) write(p string, off uint64, data []byte) (uint32, error) {
	if off > math.MaxInt64 {
		return 0, errorf(CodeBadRequest, "write offset out of range")
	}
	f, err := c.share.FS.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	var n int
	if w, ok := f.(io.WriterAt); ok {
		n, err = w.WriteAt(data, int64(off))
	} else if _, err = f.Seek(int64(off), io.SeekStart); err == nil {
		n, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return uint32(n), err
}

// setattr applies the present attributes: size, mode, owner, then mtime.
func (c *conn) setattr(p string, req *request) error {
	if req.Size != nil {
		if *req.Size > math.MaxInt64 {
			return errorf(CodeTooBig, "size out of range")
		}
		f, err := c.share.FS.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		err = f.Truncate(int64(*req.Size))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if req.Mode == nil && req.UID == nil && req.GID == nil && req.Mtime == nil {
		return nil
	}
	ch, ok := c.share.FS.(billy.Change)
	if !ok {
		return errorf(CodeUnsupported, "share cannot change attributes")
	}
	if req.Mode != nil {
		if err := ch.Chmod(p, fileMode(*req.Mode)); err != nil {
			return err
		}
	}
	if req.UID != nil || req.GID != nil {
		u, g := -1, -1
		if req.UID != nil {
			u = int(toHost(c.opts.UIDs, *req.UID))
		}
		if req.GID != nil {
			g = int(toHost(c.opts.GIDs, *req.GID))
		}
		if err := ch.Chown(p, u, g); err != nil {
			return err
		}
	}
	if req.Mtime != nil {
		secs := int64(min(*req.Mtime, math.MaxInt64))
		if err := ch.Chtimes(p, time.Time{}, time.Unix(secs, 0)); err != nil {
			return err
		}
	}
	return nil
}

// syncer is a file that can flush its data to stable storage.
type syncer interface{ Sync() error }

// sync flushes p's written data; directories need nothing.
func (c *conn) sync(p string) error {
	info, err := c.share.FS.Lstat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := c.share.FS.Open(p)
	if err != nil {
		return err
	}
	if s, ok := f.(syncer); ok {
		err = s.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// remove deletes p, which must be a directory exactly when dir is set.
func (c *conn) remove(p string, dir bool) error {
	info, err := c.share.FS.Lstat(p)
	if err != nil {
		return err
	}
	switch {
	case dir && !info.IsDir():
		return errorf(CodeNotDir, "rmdir of a non-directory: "+p)
	case !dir && info.IsDir():
		return errorf(CodeIsDir, "unlink of a directory: "+p)
	}
	return c.share.FS.Remove(p)
}

// wireMode returns the permission, setuid, setgid and sticky bits of m in Unix encoding.
func wireMode(m os.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// fileMode converts Unix permission, setuid, setgid and sticky bits to an os.FileMode.
func fileMode(bits uint32) os.FileMode {
	m := os.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if bits&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if bits&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// unixSeconds returns whole seconds since the epoch, clamped at zero.
func unixSeconds(t time.Time) uint64 {
	return uint64(max(t.Unix(), 0))
}
