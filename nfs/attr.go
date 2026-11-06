package nfs

import (
	"encoding/binary"
	"os"
	"time"

	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// fileType is an ftype3 or nfs_ftype4; the two number these types alike.
type fileType uint32

// File types.
const (
	typeReg fileType = 1 + iota
	typeDir
	typeBlk
	typeChr
	typeLnk
	typeSock
	typeFifo
)

// attrs is the NFS view of one file, from which fattr3 and NFSv4
// attributes are encoded.
type attrs struct {
	typ          fileType
	mode         uint32 // permission, setuid, setgid and sticky bits
	nlink        uint32
	uid, gid     uint32
	size         uint64 // also reported as the space used
	major, minor uint32
	fileid       uint64
	// mtime is also reported as atime and ctime, as by every ovnfs frontend;
	// the backend keeps no separate change time for every layer.
	mtime time.Time
}

// attrsOf describes file id, whose Lstat is info.
func attrsOf(id uint64, info os.FileInfo) attrs {
	a := attrs{
		typ:    typeOf(info.Mode()),
		mode:   posixMode(info.Mode()),
		nlink:  1,
		size:   uint64(max(info.Size(), 0)),
		fileid: id,
		mtime:  info.ModTime(),
	}
	if st, ok := unixstat.Of(info); ok {
		if st.Nlink > 0 {
			a.nlink = st.Nlink
		}
		a.uid, a.gid = st.UID, st.GID
		a.major, a.minor = st.Major, st.Minor
	}
	return a
}

// stat describes file id, at p, without following a final symlink.
func (s *Server) stat(id uint64, p string) (attrs, error) {
	info, err := s.FS.Lstat(p)
	if err != nil {
		return attrs{}, err
	}
	return attrsOf(id, info), nil
}

// readlink returns the target of the symlink at p; INVAL when p is not a
// symlink, which hosts report with differing errors (EINVAL on Unix,
// ERROR_NOT_A_REPARSE_POINT on Windows).
func (s *Server) readlink(p string) (string, status) {
	target, err := s.FS.Readlink(p)
	if err != nil {
		if info, lerr := s.FS.Lstat(p); lerr == nil && info.Mode()&os.ModeSymlink == 0 {
			return "", stInval
		}
		return "", statusOf(err)
	}
	return target, stOK
}

// typeOf classifies a Go file mode.
func typeOf(m os.FileMode) fileType {
	switch {
	case m.IsDir():
		return typeDir
	case m&os.ModeSymlink != 0:
		return typeLnk
	case m&os.ModeCharDevice != 0:
		return typeChr
	case m&os.ModeDevice != 0:
		return typeBlk
	case m&os.ModeSocket != 0:
		return typeSock
	case m&os.ModeNamedPipe != 0:
		return typeFifo
	}
	return typeReg
}

// posixMode encodes a Go file mode's permission and special bits as mode3
// and mode4 do (07777).
func posixMode(m os.FileMode) uint32 {
	v := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		v |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		v |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		v |= 0o1000
	}
	return v
}

// goMode decodes a mode3 or mode4 for Chmod and file creation.
func goMode(v uint32) os.FileMode {
	m := os.FileMode(v) & os.ModePerm
	if v&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if v&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if v&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// fhSize is the length of every file handle this server issues: the file id,
// big-endian.
const fhSize = 8

// fileID decodes a file handle; ok is false for one this server cannot have
// issued (NFS3ERR_BADHANDLE, NFS4ERR_BADHANDLE).
func fileID(fh []byte) (id uint64, ok bool) {
	if len(fh) != fhSize {
		return 0, false
	}
	return binary.BigEndian.Uint64(fh), true
}
