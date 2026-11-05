package nfs

import (
	"errors"
	"io"
	"math"
	"os"

	"github.com/go-git/go-billy/v5"
)

// NFSv4.0 READ, WRITE, COMMIT and SETATTR.

// stable_how4 values. Every WRITE is answered FILE_SYNC: the backend has no
// unstable writes to commit.
const (
	stable4FileSync = 2
)

// regular4 checks that the file at p is a regular file for READ or WRITE,
// returning its size.
func (s *Server) regular4(p string) (int64, status) {
	info, err := s.FS.Lstat(p)
	if err != nil {
		return 0, statusOf(err)
	}
	switch {
	case info.IsDir():
		return 0, stIsDir
	case !info.Mode().IsRegular():
		return 0, stInval
	}
	return info.Size(), stOK
}

// read answers READ, reading into the reply in place.
func (x *v4compound) read() status {
	id := getStateID4(x.d)
	offset := x.d.u64()
	count := x.d.u32()
	if x.d.err != nil {
		return stBadXDR
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	client, _, s := x.s.v4.checkIO(id, cur.id, false)
	if s != stOK {
		return s
	}
	x.note(client)
	size, s := x.s.regular4(cur.path)
	if s != stOK {
		return s
	}
	// The data must fit the reply's budget, after eof, its length and its
	// padding: a short read, which the client continues.
	n := min(int(min(count, maxIO)), x.room()-12)
	if count > 0 && n <= 0 {
		return stResource
	}
	if n <= 0 || offset >= uint64(size) {
		x.e.bool(offset >= uint64(size))
		x.e.u32(0)
		return stOK
	}
	eofAt := x.e.len()
	x.e.u32(0)
	f, err := x.s.FS.Open(cur.path)
	if err != nil {
		return statusOf(err)
	}
	defer f.Close()
	got, err := x.e.opaqueFrom(n, func(b []byte) (int, error) { return f.ReadAt(b, int64(offset)) })
	eof := errors.Is(err, io.EOF)
	if err != nil && !eof {
		return statusOf(err)
	}
	if eof || offset+uint64(got) >= uint64(size) {
		x.e.put32(eofAt, 1)
	}
	return stOK
}

// write answers WRITE: through the kept file of a write open whose file
// lost its owner's write permission (keepWritable), else through a file
// opened for the write. A SETATTR taking the permission may come between
// the two, making the open fail; the kept file it left serves then.
func (x *v4compound) write() status {
	id := getStateID4(x.d)
	offset := x.d.u64()
	stable := x.d.u32()
	data := x.d.opaque(maxRecord)
	if x.d.err != nil {
		return stBadXDR
	}
	if stable > stable4FileSync {
		return stInval
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	st := &x.s.v4
	client, kept, s := st.checkIO(id, cur.id, true)
	if s != stOK {
		return s
	}
	defer func() { st.release(kept) }()
	x.note(client)
	if s := x.conflict(cur.id); s != stOK {
		return s
	}
	if _, s := x.s.regular4(cur.path); s != stOK {
		return s
	}
	if offset > math.MaxInt64-uint64(len(data)) {
		return stFBig
	}
	var n int
	var err error
	if kept != nil {
		n, err = writeAt(kept.f, data, int64(offset))
	} else {
		n, err = x.s.writeFile4(cur.path, data, int64(offset))
		if errors.Is(err, os.ErrPermission) && client != 0 {
			if kept = st.kept(id, cur.id); kept != nil {
				n, err = writeAt(kept.f, data, int64(offset))
			}
		}
	}
	if err != nil {
		return statusOf(err)
	}
	x.changed(cur.id)
	x.e.u32(uint32(n))
	x.e.u32(stable4FileSync)
	x.e.fixed(x.s.verf[:])
	return stOK
}

// writeFile4 writes data at offset of the file at p.
func (s *Server) writeFile4(p string, data []byte, offset int64) (int, error) {
	f, err := s.FS.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	n, err := writeAt(f, data, offset)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// commit answers COMMIT: every WRITE was FILE_SYNC, so there is nothing to
// commit and the verifier is all it returns.
func (x *v4compound) commit() status {
	x.d.u64() // offset
	x.d.u32() // count
	if x.d.err != nil {
		return stBadXDR
	}
	if _, s := x.current(); s != stOK {
		return s
	}
	x.e.fixed(x.s.verf[:])
	return stOK
}

// setattr answers SETATTR with the attributes it set; SETATTR4res carries
// attrsset whatever the status, empty on failure.
func (x *v4compound) setattr() status {
	set, s := x.setattrs()
	if s != stOK {
		set = v4bitmap{}
	}
	set.put(x.e)
	return s
}

// setattrs sets the current file's attributes. A delegation stateid may
// not change the size; any change recalls other clients' delegations.
func (x *v4compound) setattrs() (v4bitmap, status) {
	id := getStateID4(x.d)
	sa, s := getSattr4(x.d)
	if s != stOK {
		return v4bitmap{}, s
	}
	cur, s := x.current()
	if s != stOK {
		return v4bitmap{}, s
	}
	// kept, the file kept for a write open of a file that lost its owner's
	// write permission before, truncates it. A mode this SETATTR sets
	// applies after the size (apply4), so the file it keeps now is not
	// needed for that.
	client, kept, s := x.s.v4.checkIO(id, cur.id, sa.mask.has(attr4Size))
	if s != stOK {
		return v4bitmap{}, s
	}
	defer x.s.v4.release(kept)
	x.note(client)
	if s := x.conflict(cur.id); s != stOK {
		return v4bitmap{}, s
	}
	if sa.mask.has(attr4Mode) {
		x.keepWritable(cur, sa.mode)
	}
	var kf billy.File
	if kept != nil {
		kf = kept.f
	}
	if s := x.s.apply4(cur.path, &sa, kf); s != stOK {
		return v4bitmap{}, s
	}
	x.changed(cur.id)
	return sa.mask, stOK
}
