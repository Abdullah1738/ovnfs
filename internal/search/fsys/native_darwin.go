//go:build darwin

package fsys

import (
	"encoding/binary"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin directory reads:
//
//   - Children open with openat(parent, name): one-component lookups instead
//     of re-resolving the whole path from the root on every directory.
//   - Minimal listings use __getdirentries64 with a buffer of at least
//     GETDIRENTRIES64_EXTENDED_BUFSIZE; the kernel then stores an EOF flag in
//     the buffer's last four bytes, so a directory that fits in the buffer
//     takes exactly one listing call instead of a second call returning 0.
//     On APFS this also beats getattrlistbulk(NAME|OBJTYPE) by ~25%.
//   - Full listings use getattrlistbulk, which returns names, types, mtimes,
//     and sizes without per-entry stat calls. The parent listing also returns
//     each subdirectory's entry count, so the child listing stops once it has
//     seen that many entries rather than issuing a final empty call.

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

var (
	libc_openat_trampoline_addr          uintptr
	libc_getdirentries64_trampoline_addr uintptr
	libc_getattrlistbulk_trampoline_addr uintptr
	libc_fstatat_trampoline_addr         uintptr
)

//go:cgo_import_dynamic libc_openat openat "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_getdirentries64 __getdirentries64 "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_getattrlistbulk getattrlistbulk "/usr/lib/libSystem.B.dylib"

const (
	listBufSize = 128 << 10

	// defaultWorkers caps the default worker count. Directory opens and
	// listings contend in the kernel: on APFS throughput peaks at 4–6
	// threads and collapses beyond 8 (16 threads run ~1.7× slower than 4).
	defaultWorkers = 4

	// struct dirent (64-bit inode layout) field offsets.
	direntIno    = 0
	direntReclen = 16
	direntNamlen = 18
	direntType   = 20
	direntName   = 21

	getdirentries64EOF = 1

	attrBitMapCount     = 5
	attrCmnName         = 0x00000001
	attrCmnObjType      = 0x00000008
	attrCmnModTime      = 0x00000400
	attrCmnReturnedAttr = 0x80000000
	attrDirEntryCount   = 0x00000002
	attrFileDataLength  = 0x00000200
	fsoptNoFollow       = 0x00000001

	// getattrlistbulk record layout: u32 length, the 20-byte returned
	// attribute_set_t (common, vol, dir, file, fork), then each returned
	// attribute in bitmap order, 4-byte aligned. getattrlistbulk rejects
	// requests without ATTR_CMN_RETURNED_ATTRS (EINVAL) and omits attributes
	// that do not apply to an entry, so later offsets depend on the set.
	bulkReturnedCommon = 4
	bulkReturnedDir    = 12
	bulkReturnedFile   = 16
	bulkNameRef        = 24
	bulkObjType        = 32
	bulkModTime        = 36
	bulkMinLen         = 52

	vreg = 1
	vdir = 2
	vlnk = 5
)

type attrList struct {
	bitmapCount uint16
	reserved    uint16
	commonAttr  uint32
	volAttr     uint32
	dirAttr     uint32
	fileAttr    uint32
	forkAttr    uint32
}

var fullAttrs = attrList{
	bitmapCount: attrBitMapCount,
	commonAttr:  attrCmnReturnedAttr | attrCmnName | attrCmnObjType | attrCmnModTime,
	dirAttr:     attrDirEntryCount,
	fileAttr:    attrFileDataLength,
}

func openat(dir int, cname []byte, flags int) (int, error) {
	for {
		r, _, e := syscall_syscall6(libc_openat_trampoline_addr, uintptr(dir), uintptr(unsafe.Pointer(&cname[0])), uintptr(flags), 0, 0, 0)
		switch e {
		case 0:
			return int(r), nil
		case unix.EINTR:
			continue
		}
		return -1, e
	}
}

// list reads every entry of h into s.ls. count is the directory's entry
// count from its parent's Full listing (0 when unknown).
func (s *session) list(h handle, detail Detail, count uint32) error {
	if len(s.buf) < listBufSize {
		s.buf = make([]byte, listBufSize)
	}
	if detail == Full {
		return s.listBulk(h, count)
	}
	return s.listDirents(h)
}

func (s *session) listDirents(h handle) error {
	buf := s.buf
	var base int64
	for {
		r, _, e := syscall_syscall6(libc_getdirentries64_trampoline_addr, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&base)), 0, 0)
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		n := int(r)
		if n <= 0 {
			return nil
		}
		for off := 0; off+direntName <= n; {
			rec := buf[off:n]
			reclen := int(binary.LittleEndian.Uint16(rec[direntReclen:]))
			if reclen < direntName || reclen > len(rec) {
				return unix.EIO
			}
			off += reclen
			namlen := int(binary.LittleEndian.Uint16(rec[direntNamlen:]))
			if direntName+namlen > reclen || binary.LittleEndian.Uint64(rec[direntIno:]) == 0 {
				continue
			}
			name := rec[direntName : direntName+namlen]
			if isDotOrDotDot(name) {
				continue
			}
			typ := typeFromDT(rec[direntType])
			if typ == 0 {
				if rec[direntType] != unix.DT_UNKNOWN {
					continue
				}
				if typ = statType(h, name); typ == 0 {
					continue
				}
			}
			s.ls.add(name, typ, -1, 0, 0)
		}
		if binary.LittleEndian.Uint32(buf[len(buf)-4:])&getdirentries64EOF != 0 {
			return nil
		}
	}
}

func (s *session) listBulk(h handle, count uint32) error {
	buf := s.buf
	seen := uint32(0)
	for {
		r, _, e := syscall_syscall6(libc_getattrlistbulk_trampoline_addr, uintptr(h), uintptr(unsafe.Pointer(&fullAttrs)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), fsoptNoFollow, 0)
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		n := int(r)
		if n <= 0 {
			return nil
		}
		off := 0
		for range n {
			if off+bulkMinLen > len(buf) {
				return unix.EIO
			}
			rec := buf[off:]
			reclen := int(binary.LittleEndian.Uint32(rec))
			if reclen < bulkMinLen || reclen > len(rec) {
				return unix.EIO
			}
			rec = rec[:reclen]
			off += reclen
			const wantCommon = attrCmnName | attrCmnObjType | attrCmnModTime
			if binary.LittleEndian.Uint32(rec[bulkReturnedCommon:])&wantCommon != wantCommon {
				continue
			}
			nameOff := bulkNameRef + int(int32(binary.LittleEndian.Uint32(rec[bulkNameRef:])))
			nameLen := int(binary.LittleEndian.Uint32(rec[bulkNameRef+4:]))
			if nameOff < bulkMinLen || nameLen == 0 || nameOff+nameLen > reclen {
				return unix.EIO
			}
			name := rec[nameOff : nameOff+nameLen]
			if name[len(name)-1] == 0 {
				name = name[:len(name)-1]
			}
			if len(name) == 0 || isDotOrDotDot(name) {
				continue
			}
			var typ Type
			switch binary.LittleEndian.Uint32(rec[bulkObjType:]) {
			case vreg:
				typ = File
			case vdir:
				typ = Dir
			case vlnk:
				typ = Symlink
			default:
				continue
			}
			sec := int64(binary.LittleEndian.Uint64(rec[bulkModTime:]))
			nsec := int64(binary.LittleEndian.Uint64(rec[bulkModTime+8:]))
			size := int64(-1)
			var children uint32
			cursor := bulkMinLen
			if binary.LittleEndian.Uint32(rec[bulkReturnedDir:])&attrDirEntryCount != 0 && cursor+4 <= nameOff {
				children = binary.LittleEndian.Uint32(rec[cursor:])
				cursor += 4
			}
			if binary.LittleEndian.Uint32(rec[bulkReturnedFile:])&attrFileDataLength != 0 && cursor+8 <= nameOff && typ == File {
				size = int64(binary.LittleEndian.Uint64(rec[cursor:]))
			}
			s.ls.add(name, typ, size, sec*1e9+nsec, children)
		}
		seen += uint32(n)
		if count > 0 && seen >= count {
			return nil
		}
	}
}

// Stat allocates nothing, unlike unix.Fstatat.
func (s *session) Stat(path string, follow bool) (FileInfo, error) {
	flags := uintptr(unix.AT_SYMLINK_NOFOLLOW)
	if follow {
		flags = 0
	}
	cname := s.cstr(path)
	cwd := unix.AT_FDCWD
	var st unix.Stat_t
	for {
		_, _, e := syscall_syscall6(libc_fstatat_trampoline_addr, uintptr(cwd), uintptr(unsafe.Pointer(&cname[0])), uintptr(unsafe.Pointer(&st)), flags, 0, 0)
		switch e {
		case 0:
			return FileInfo{Type: typeFromMode(uint32(st.Mode)), Size: st.Size, Stamp: stampOf(&st)}, nil
		case unix.EINTR:
			continue
		}
		return FileInfo{}, e
	}
}

// statHandle stamps an open descriptor and reports its type and size.
func statHandle(h handle) (Stamp, Type, int64, error) {
	var st unix.Stat_t
	err := unix.Fstat(h, &st)
	return stampOf(&st), typeFromMode(uint32(st.Mode)), st.Size, err
}

func stampOf(st *unix.Stat_t) Stamp {
	return Stamp{Dev: uint64(uint32(st.Dev)), Ino: st.Ino, Mtime: st.Mtim.Nano(), Ctime: st.Ctim.Nano()}
}

// fineTimestamps admits APFS. HFS+ keeps 1-second timestamps; network and
// FUSE filesystems may not update directory times promptly.
func fineTimestamps(h handle) bool {
	var fs unix.Statfs_t
	return unix.Fstatfs(h, &fs) == nil && unix.ByteSliceToString(fs.Fstypename[:]) == "apfs"
}

// statType resolves an entry whose listing reported DT_UNKNOWN.
func statType(dir handle, name []byte) Type {
	var st unix.Stat_t
	if unix.Fstatat(dir, string(name), &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return 0
	}
	return typeFromMode(uint32(st.Mode))
}
