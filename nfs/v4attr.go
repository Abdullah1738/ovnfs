package nfs

import (
	"encoding/binary"
	"iter"
	"math"
	"math/bits"
	"os"
	"strconv"
	"time"

	"github.com/go-git/go-billy/v5"

	"github.com/stencil-hq/ovnfs/smb"
)

// NFSv4 file attributes (RFC 7530 section 5): GETATTR, READDIR and VERIFY
// encode them from attrs; SETATTR, CREATE and OPEN decode and apply the
// settable ones.

// Attribute numbers.
const (
	attr4SupportedAttrs  = 0
	attr4Type            = 1
	attr4FHExpireType    = 2
	attr4Change          = 3
	attr4Size            = 4
	attr4LinkSupport     = 5
	attr4SymlinkSupport  = 6
	attr4NamedAttr       = 7
	attr4FSID            = 8
	attr4UniqueHandles   = 9
	attr4LeaseTime       = 10
	attr4RDAttrError     = 11
	attr4ACLSupport      = 13
	attr4CanSetTime      = 15
	attr4CaseInsensitive = 16
	attr4CasePreserving  = 17
	attr4ChownRestricted = 18
	attr4FileHandle      = 19
	attr4FileID          = 20
	attr4FilesAvail      = 21
	attr4FilesFree       = 22
	attr4FilesTotal      = 23
	attr4Homogeneous     = 26
	attr4MaxFileSize     = 27
	attr4MaxLink         = 28
	attr4MaxName         = 29
	attr4MaxRead         = 30
	attr4MaxWrite        = 31
	attr4Mode            = 33
	attr4NoTrunc         = 34
	attr4NumLinks        = 35
	attr4Owner           = 36
	attr4OwnerGroup      = 37
	attr4RawDev          = 41
	attr4SpaceAvail      = 42
	attr4SpaceFree       = 43
	attr4SpaceTotal      = 44
	attr4SpaceUsed       = 45
	attr4TimeAccess      = 47
	attr4TimeAccessSet   = 48
	attr4TimeDelta       = 51
	attr4TimeMetadata    = 52
	attr4TimeModify      = 53
	attr4TimeModifySet   = 54
	attr4MountedOnFileID = 55
)

// v4MaxBitmap bounds the words of a bitmap4 argument.
const v4MaxBitmap = 8

var (
	// attr4Supported is supported_attrs. It lists the write-only
	// time_access_set and time_modify_set: clients set only attributes a
	// server says it supports (Linux would never send times, and touch -d
	// would do nothing). hidden and system are left out, as the backend
	// cannot keep them: the macOS client masks the attributes it creates
	// files with (mode, hidden, archive) by supported_attrs, and an
	// advertised hidden it cannot store failed every create with
	// NFS4ERR_ATTRNOTSUPP; chflags hidden now fails on the client instead.
	attr4Supported = bitmap4(
		attr4SupportedAttrs, attr4Type, attr4FHExpireType, attr4Change, attr4Size,
		attr4LinkSupport, attr4SymlinkSupport, attr4NamedAttr, attr4FSID,
		attr4UniqueHandles, attr4LeaseTime, attr4RDAttrError, attr4ACLSupport,
		attr4CanSetTime, attr4CaseInsensitive, attr4CasePreserving,
		attr4ChownRestricted, attr4FileHandle, attr4FileID, attr4FilesAvail,
		attr4FilesFree, attr4FilesTotal, attr4Homogeneous, attr4MaxFileSize,
		attr4MaxLink, attr4MaxName, attr4MaxRead, attr4MaxWrite, attr4Mode,
		attr4NoTrunc, attr4NumLinks, attr4Owner, attr4OwnerGroup, attr4RawDev,
		attr4SpaceAvail, attr4SpaceFree, attr4SpaceTotal, attr4SpaceUsed,
		attr4TimeAccess, attr4TimeAccessSet, attr4TimeDelta, attr4TimeMetadata,
		attr4TimeModify, attr4TimeModifySet, attr4MountedOnFileID,
	)
	// attr4Writable are the attributes SETATTR, CREATE and OPEN set.
	attr4Writable = bitmap4(attr4Size, attr4Mode, attr4Owner, attr4OwnerGroup,
		attr4TimeAccessSet, attr4TimeModifySet)
	// attr4WriteOnly are set but never reported.
	attr4WriteOnly = bitmap4(attr4TimeAccessSet, attr4TimeModifySet)
	// attr4Space are the attributes Backend.Statfs reports.
	attr4Space = bitmap4(attr4FilesAvail, attr4FilesFree, attr4FilesTotal,
		attr4SpaceAvail, attr4SpaceFree, attr4SpaceTotal)
)

// v4bitmap is a bitmap4 of the attribute numbers this server knows, all
// below 64.
type v4bitmap [2]uint32

// bitmap4 makes a bitmap of attributes.
func bitmap4(attrs ...uint32) v4bitmap {
	var b v4bitmap
	for _, a := range attrs {
		b[a/32] |= 1 << (a % 32)
	}
	return b
}

// has reports whether attribute a is in the bitmap.
func (b v4bitmap) has(a uint32) bool { return a < 64 && b[a/32]&(1<<(a%32)) != 0 }

// and is the intersection of two bitmaps.
func (b v4bitmap) and(o v4bitmap) v4bitmap { return v4bitmap{b[0] & o[0], b[1] & o[1]} }

// andNot is b without o's attributes.
func (b v4bitmap) andNot(o v4bitmap) v4bitmap { return v4bitmap{b[0] &^ o[0], b[1] &^ o[1]} }

// intersects reports whether b and o share an attribute.
func (b v4bitmap) intersects(o v4bitmap) bool { return b.and(o) != v4bitmap{} }

// within reports whether every attribute of b is in o.
func (b v4bitmap) within(o v4bitmap) bool { return b.andNot(o) == v4bitmap{} }

// all yields the bitmap's attributes in increasing order, the order of
// their values in a fattr4.
func (b v4bitmap) all() iter.Seq[uint32] {
	return func(yield func(uint32) bool) {
		for w, word := range b {
			for ; word != 0; word &= word - 1 {
				if !yield(uint32(w*32 + bits.TrailingZeros32(word))) {
					return
				}
			}
		}
	}
}

// put appends the bitmap.
func (b v4bitmap) put(e *encoder) {
	e.u32(2)
	e.u32(b[0])
	e.u32(b[1])
}

// getBitmap4 decodes a bitmap4; extra reports attributes beyond those this
// server knows.
func getBitmap4(d *decoder) (b v4bitmap, extra bool) {
	n := d.u32()
	if n > v4MaxBitmap {
		d.fail()
		return v4bitmap{}, false
	}
	for i := range n {
		w := d.u32()
		if i < 2 {
			b[i] = w
		} else if w != 0 {
			extra = true
		}
	}
	return b, extra
}

// v4fsstat holds Backend.Statfs for the attributes of one reply, asked for
// once and only if an attribute needs it.
type v4fsstat struct {
	smb.Statfs
	err  error
	done bool
}

// get asks the backend once.
func (f *v4fsstat) get(s *Server) error {
	if !f.done {
		f.Statfs, f.err = s.FS.Statfs()
		f.done = true
	}
	return f.err
}

// reported4 is the attributes of req a reply reports: the supported ones
// but the write-only.
func reported4(req v4bitmap) v4bitmap { return req.and(attr4Supported).andNot(attr4WriteOnly) }

// putAttrs4 appends the fattr4 of the file a describes: the requested
// attributes this server reports, and their values.
func (s *Server) putAttrs4(e *encoder, a *attrs, req v4bitmap, fs *v4fsstat) status {
	mask := reported4(req)
	mask.put(e)
	at := e.len()
	e.u32(0)
	if st := s.putAttrVals4(e, a, mask, fs); st != stOK {
		return st
	}
	e.put32(at, uint32(e.len()-at-4))
	return stOK
}

// putAttrVals4 appends the values of the attributes in mask, which must be
// reportable ones, as a fattr4's attr_vals holds them.
func (s *Server) putAttrVals4(e *encoder, a *attrs, mask v4bitmap, fs *v4fsstat) status {
	if mask.intersects(attr4Space) {
		if err := fs.get(s); err != nil {
			return statusOf(err)
		}
	}
	block := uint64(fs.BlockSize)
	for attr := range mask.all() {
		switch attr {
		case attr4SupportedAttrs:
			attr4Supported.put(e)
		case attr4Type:
			e.u32(uint32(a.typ))
		case attr4FHExpireType:
			e.u32(0) // FH4_PERSISTENT: handles are file ids
		case attr4Change:
			e.u64(v4Change(a))
		case attr4Size, attr4SpaceUsed:
			e.u64(a.size)
		case attr4LinkSupport, attr4SymlinkSupport, attr4UniqueHandles, attr4CanSetTime,
			attr4CasePreserving, attr4ChownRestricted, attr4Homogeneous, attr4NoTrunc:
			e.bool(true)
		case attr4NamedAttr, attr4CaseInsensitive:
			e.bool(false)
		case attr4FSID:
			e.u64(0)
			e.u64(1)
		case attr4LeaseTime:
			e.u32(uint32(s.v4.lease / time.Second))
		case attr4RDAttrError:
			e.u32(uint32(stOK))
		case attr4ACLSupport:
			e.u32(0)
		case attr4FileHandle:
			e.fh(a.fileid)
		case attr4FileID, attr4MountedOnFileID:
			e.u64(a.fileid)
		case attr4FilesAvail, attr4FilesFree:
			e.u64(fs.FilesFree)
		case attr4FilesTotal:
			e.u64(fs.Files)
		case attr4MaxFileSize:
			e.u64(math.MaxInt64)
		case attr4MaxLink:
			e.u32(maxLink)
		case attr4MaxName:
			e.u32(maxName)
		case attr4MaxRead, attr4MaxWrite:
			e.u64(maxIO)
		case attr4Mode:
			e.u32(a.mode)
		case attr4NumLinks:
			e.u32(a.nlink)
		case attr4Owner:
			putOwner4(e, a.uid)
		case attr4OwnerGroup:
			putOwner4(e, a.gid)
		case attr4RawDev:
			e.u32(a.major)
			e.u32(a.minor)
		case attr4SpaceAvail:
			e.u64(fs.Avail * block)
		case attr4SpaceFree:
			e.u64(fs.Free * block)
		case attr4SpaceTotal:
			e.u64(fs.Blocks * block)
		case attr4TimeAccess, attr4TimeMetadata, attr4TimeModify:
			putTime4(e, a.mtime)
		case attr4TimeDelta:
			e.u64(0)
			e.u32(1)
		}
	}
	return stOK
}

// putOwner4 appends an owner or owner_group: the numeric id as a string,
// which clients map without an id-mapping service.
func putOwner4(e *encoder, id uint32) {
	var b [10]byte
	e.opaque(strconv.AppendUint(b[:0], uint64(id), 10))
}

// putTime4 appends an nfstime4.
func putTime4(e *encoder, t time.Time) {
	e.u64(uint64(t.Unix()))
	e.u32(uint32(t.Nanosecond()))
}

// v4Change stands in for the change attribute, which the backend does not
// keep and which cannot be a server counter, as files also change outside
// the server. It hashes what a change moves: modification time, type and
// mode, owner and group, and, but for directories, size and link count.
//
// A directory's size and link count follow its backing entries, which the
// backend may rearrange without a visible change (ovnfs moving a file from
// memory to disk keeps the directory's mtime). Clients purge their cached
// names whenever a directory's change moves, so for directories it follows
// the mtime, which every entry change moves, and metadata alone.
func v4Change(a *attrs) uint64 {
	var b [32]byte
	binary.BigEndian.PutUint64(b[0:], uint64(a.mtime.UnixNano()))
	binary.BigEndian.PutUint32(b[8:], uint32(a.typ)<<16|a.mode)
	binary.BigEndian.PutUint32(b[12:], a.uid)
	binary.BigEndian.PutUint32(b[16:], a.gid)
	if a.typ != typeDir {
		binary.BigEndian.PutUint64(b[20:], a.size)
		binary.BigEndian.PutUint32(b[28:], a.nlink)
	}
	// FNV-1a, stable across restarts so clients keep their caches.
	h := uint64(14695981039346656037)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// changeAt4 is the change attribute of the file at p, 0 if it is gone.
func (s *Server) changeAt4(p string) uint64 {
	a, err := s.stat(s.FS.FileID(p), p)
	if err != nil {
		return 0
	}
	return v4Change(&a)
}

// v4sattr is a decoded fattr4 of attributes to set (SETATTR, and the
// attributes CREATE and OPEN create files with). mask is attrsset.
type v4sattr struct {
	mask         v4bitmap
	size         uint64
	mode         uint32
	uid, gid     uint32
	atime, mtime time.Time
}

// Settime4 choices.
const (
	time4Server = 0
	time4Client = 1
)

// getSattr4 decodes a fattr4 of attributes to set. Attributes that cannot be
// set are NFS4ERR_ATTRNOTSUPP; owners are numeric ids, as GETATTR reports
// them.
func getSattr4(d *decoder) (v4sattr, status) {
	mask, extra := getBitmap4(d)
	vals := d.opaque(v4ArgLimit)
	if d.err != nil {
		return v4sattr{}, stBadXDR
	}
	if extra || !mask.within(attr4Writable) {
		return v4sattr{}, stAttrNotSupp
	}
	sa := v4sattr{mask: mask}
	vd := decoder{buf: vals}
	for attr := range mask.all() {
		switch attr {
		case attr4Size:
			sa.size = vd.u64()
		case attr4Mode:
			sa.mode = vd.u32()
		case attr4Owner, attr4OwnerGroup:
			id, err := strconv.ParseUint(vd.str(v4OpaqueLimit), 10, 32)
			if vd.err != nil {
				return v4sattr{}, stBadXDR
			}
			if err != nil {
				return v4sattr{}, stBadOwner
			}
			if attr == attr4Owner {
				sa.uid = uint32(id)
			} else {
				sa.gid = uint32(id)
			}
		case attr4TimeAccessSet, attr4TimeModifySet:
			var t time.Time
			switch vd.u32() {
			case time4Server:
				t = time.Now()
			case time4Client:
				sec, nsec := int64(vd.u64()), vd.u32()
				if nsec >= 1e9 {
					return v4sattr{}, stInval
				}
				t = time.Unix(sec, int64(nsec))
			default:
				if vd.err == nil {
					return v4sattr{}, stInval
				}
			}
			if attr == attr4TimeAccessSet {
				sa.atime = t
			} else {
				sa.mtime = t
			}
		}
	}
	if vd.err != nil {
		return v4sattr{}, stBadXDR
	}
	return sa, stOK
}

// apply4 sets the attributes sa lists on the file at p. kept, if not nil,
// is the file kept open for writing (see keepWritable), which truncates
// the file once its mode no longer lets it be opened for writing.
//
// The size goes first, while the mode still allows writing, and the owner
// before the mode, since a chown clears setuid and setgid bits. A symlink's
// mode is left alone: its bits mean nothing, and Chmod would follow it to
// the target, or fail with ENOENT when it dangles.
func (s *Server) apply4(p string, sa *v4sattr, kept billy.File) status {
	if sa.mask == (v4bitmap{}) {
		return stOK
	}
	a, err := s.stat(s.FS.FileID(p), p)
	if err != nil {
		return statusOf(err)
	}
	if sa.mask.has(attr4Size) {
		switch {
		case a.typ == typeDir:
			return stIsDir
		case a.typ != typeReg:
			return stInval
		case sa.size > math.MaxInt64:
			return stFBig
		}
		if kept != nil {
			err = kept.Truncate(int64(sa.size))
		} else {
			err = s.truncate4(p, int64(sa.size))
		}
		if err != nil {
			return statusOf(err)
		}
	}
	if sa.mask.has(attr4Owner) || sa.mask.has(attr4OwnerGroup) {
		uid, gid := a.uid, a.gid
		if sa.mask.has(attr4Owner) {
			uid = sa.uid
		}
		if sa.mask.has(attr4OwnerGroup) {
			gid = sa.gid
		}
		if uid != a.uid || gid != a.gid {
			if err := s.FS.Lchown(p, int(uid), int(gid)); err != nil {
				return statusOf(err)
			}
		}
	}
	if sa.mask.has(attr4Mode) && a.typ != typeLnk && sa.mode&0o7777 != a.mode {
		if err := s.FS.Chmod(p, goMode(sa.mode)); err != nil {
			return statusOf(err)
		}
	}
	if sa.mask.has(attr4TimeAccessSet) || sa.mask.has(attr4TimeModifySet) {
		// The access time is reported as the modification time, so that is
		// what an unset one keeps.
		atime, mtime := a.mtime, a.mtime
		if sa.mask.has(attr4TimeAccessSet) {
			atime = sa.atime
		}
		if sa.mask.has(attr4TimeModifySet) {
			mtime = sa.mtime
		}
		if err := s.FS.Chtimes(p, atime, mtime); err != nil {
			return statusOf(err)
		}
	}
	return stOK
}

// truncate4 sets the size of the file at p.
func (s *Server) truncate4(p string, size int64) error {
	f, err := s.FS.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	err = f.Truncate(size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
