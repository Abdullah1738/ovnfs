//go:build windows

package fsys

import (
	"encoding/binary"
	"errors"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows directory reads:
//
//   - Children open with NtCreateFile relative to the parent's handle
//     (OBJECT_ATTRIBUTES.RootDirectory) and the raw entry name: no Win32
//     path parsing, no DOS-to-NT path conversion, no per-component
//     re-traversal from the drive root, and no MAX_PATH limit. Files read
//     through a directory handle open the same way.
//   - Listings use NtQueryDirectoryFile with FileDirectoryInformation, the
//     smallest class that carries attributes, size, and mtime, so Full detail
//     costs nothing extra and each call returns as many entries as possible.
//   - Names convert from UTF-16 straight into the listing arena as WTF-8,
//     which round-trips unpaired surrogates back to the exact on-disk name.
//   - Stamps come from NtQueryFullAttributesFile, which answers a path query
//     without the caller opening a handle.
//
// Reparse points (symlinks, junctions, mount points, cloud placeholders) are
// reported as Symlink and never descended.

type handle = windows.Handle

var (
	ntdll                         = windows.NewLazySystemDLL("ntdll.dll")
	procNtQueryDirectoryFile      = ntdll.NewProc("NtQueryDirectoryFile")
	procNtQueryFullAttributesFile = ntdll.NewProc("NtQueryFullAttributesFile")
)

const (
	listBufSize = 64 << 10

	// defaultWorkers caps the default worker count (unmeasured; matches
	// Linux).
	defaultWorkers = 8

	fileDirectoryInformation = 1

	// FILE_DIRECTORY_INFORMATION field offsets.
	fdiNextEntryOffset = 0
	fdiLastWriteTime   = 24
	fdiEndOfFile       = 40
	fdiFileAttributes  = 56
	fdiFileNameLength  = 60
	fdiFileName        = 64

	statusNoSuchFile = windows.NTStatus(0xC000000F)

	// 100ns ticks between 1601-01-01 and 1970-01-01.
	unixEpochTicks = 116_444_736_000_000_000

	shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

	pathSep = '\\'
	// memoStableByDev: path queries expose no volume identity (Stamp.Dev is
	// always 0), so the filesystem is checked per stored directory.
	memoStableByDev = false

	fileNetworkOpenInformation = 34

	// dirAccess lists entries and reads attributes (for stamps).
	dirAccess  = windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
	fileAccess = windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
)

// networkOpenInfo is FILE_NETWORK_OPEN_INFORMATION, shared by
// NtQueryFullAttributesFile (by path) and NtQueryInformationFile (by handle).
type networkOpenInfo struct {
	creationTime, lastAccessTime, lastWriteTime, changeTime int64
	allocationSize, endOfFile                               int64
	fileAttributes                                          uint32
	_                                                       uint32
}

func (i *networkOpenInfo) stamp() Stamp {
	return Stamp{
		Ino:   uint64(i.creationTime),
		Mtime: (i.lastWriteTime - unixEpochTicks) * 100,
		Ctime: (i.changeTime - unixEpochTicks) * 100,
	}
}

// Stat queries an absolute Win32 path without opening a handle. The query
// describes a reparse point itself, so one is reported as a Symlink, or with
// follow opened to describe its target.
func (s *session) Stat(path string, follow bool) (FileInfo, error) {
	name := path
	switch {
	case strings.HasPrefix(name, `\\?\`):
		name = name[4:]
		s.wname = appendUTF16(s.wname[:0], `\??\`)
	case strings.HasPrefix(name, `\\`):
		name = name[2:]
		s.wname = appendUTF16(s.wname[:0], `\??\UNC\`)
	default:
		s.wname = appendUTF16(s.wname[:0], `\??\`)
	}
	s.wname = appendUTF16(s.wname, name)
	if len(s.wname) > 0x7fff {
		return FileInfo{}, windows.ERROR_FILENAME_EXCED_RANGE
	}
	us := windows.NTUnicodeString{Length: uint16(2 * len(s.wname)), MaximumLength: uint16(2 * len(s.wname)), Buffer: &s.wname[0]}
	oa := windows.OBJECT_ATTRIBUTES{ObjectName: &us, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var info networkOpenInfo
	r, _, _ := syscall.SyscallN(procNtQueryFullAttributesFile.Addr(), uintptr(unsafe.Pointer(&oa)), uintptr(unsafe.Pointer(&info)))
	if status := windows.NTStatus(r); status != windows.STATUS_SUCCESS {
		return FileInfo{}, ntErr(status)
	}
	if info.fileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		if !follow {
			return FileInfo{Type: Symlink, Size: -1, Stamp: info.stamp()}, nil
		}
		return s.statTarget(path)
	}
	typ, size := File, info.endOfFile
	if info.fileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		typ, size = Dir, -1
	}
	return FileInfo{Type: typ, Size: size, Stamp: info.stamp()}, nil
}

// statTarget opens path, which resolves its reparse points, and describes what
// it reaches; a dangling link fails as not-exist.
func (s *session) statTarget(path string) (FileInfo, error) {
	h, err := s.createFile(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return FileInfo{}, err
	}
	defer windows.CloseHandle(h)
	st, typ, size, err := statHandle(h)
	if typ == Dir {
		size = -1
	}
	return FileInfo{Type: typ, Size: size, Stamp: st}, err
}

// Identify opens the path because path queries carry no file index.
func (s *session) Identify(path string) (ID, error) {
	h, err := s.createFile(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return ID{}, err
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return ID{}, err
	}
	return ID{Dev: uint64(fi.VolumeSerialNumber), Ino: uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow)}, nil
}

// statHandle stamps an open handle and reports its type and size (reparse
// points opened without FILE_OPEN_REPARSE_POINT report their target).
func statHandle(h handle) (Stamp, Type, int64, error) {
	var info networkOpenInfo
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtQueryInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), fileNetworkOpenInformation); err != nil {
		return Stamp{}, 0, 0, err
	}
	typ := File
	if info.fileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		typ = Dir
	}
	return info.stamp(), typ, info.endOfFile, nil
}

// fineTimestamps admits NTFS and ReFS, which keep 100ns timestamps and
// update a directory's write time when its entries change.
func fineTimestamps(h handle) bool {
	var name [windows.MAX_PATH + 1]uint16
	if windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, nil, &name[0], uint32(len(name))) != nil {
		return false
	}
	fs := windows.UTF16ToString(name[:])
	return fs == "NTFS" || fs == "ReFS"
}

// createFile opens an absolute path through the Win32 layer, which resolves
// drive-relative forms, UNC paths, and `\\?\` prefixes.
func (s *session) createFile(path string, access, flags uint32) (handle, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return windows.InvalidHandle, windows.ERROR_INVALID_NAME
	}
	s.wname = append(appendUTF16(s.wname[:0], longPath(path)), 0)
	return windows.CreateFile(&s.wname[0], access, shareAll, nil, windows.OPEN_EXISTING, flags, 0)
}

func (s *session) openDir(path string) (handle, error) {
	h, err := s.createFile(path, dirAccess, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return windows.InvalidHandle, err
	}
	// FILE_FLAG_BACKUP_SEMANTICS also opens regular files.
	if _, typ, _, err := statHandle(h); err != nil || typ != Dir {
		windows.CloseHandle(h)
		if err == nil {
			err = windows.ERROR_DIRECTORY
		}
		return windows.InvalidHandle, err
	}
	return h, nil
}

// longPath adds the `\\?\` prefix that lifts MAX_PATH for long absolute
// paths, as the os package does.
func longPath(path string) string {
	switch {
	case len(path) < 248 || strings.HasPrefix(path, `\\?\`):
		return path
	case strings.HasPrefix(path, `\\`):
		return `\\?\UNC\` + path[2:]
	case len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/'):
		return `\\?\` + path
	}
	return path
}

// openChild opens the subdirectory name of parent without following a
// reparse point that replaced it since the listing.
func (s *session) openChild(parent handle, name string) (handle, error) {
	return s.ntOpen(parent, name, dirAccess, windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT)
}

func (s *session) ntOpen(parent handle, name string, access, options uint32) (handle, error) {
	s.wname = appendUTF16(s.wname[:0], name)
	if len(s.wname) == 0 || len(s.wname) > 0x7fff {
		return windows.InvalidHandle, windows.ERROR_INVALID_NAME
	}
	us := windows.NTUnicodeString{
		Length:        uint16(2 * len(s.wname)),
		MaximumLength: uint16(2 * len(s.wname)),
		Buffer:        &s.wname[0],
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: &us}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h handle
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtCreateFile(&h, access, &oa, &iosb, nil, 0, shareAll, windows.FILE_OPEN,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT, 0, 0)
	if err != nil {
		return windows.InvalidHandle, ntErr(err)
	}
	return h, nil
}

// ntErr turns an NTSTATUS into the Win32 error the os package would report, so
// callers can match fs.ErrNotExist and the like; a FILE_NON_DIRECTORY_FILE open
// that found a directory is ErrNotRegular, as on Unix.
func ntErr(err error) error {
	status, ok := err.(windows.NTStatus)
	switch {
	case !ok:
		return err
	case status == windows.STATUS_FILE_IS_A_DIRECTORY:
		return ErrNotRegular
	}
	return status.Errno()
}

func closeHandle(h handle) { windows.CloseHandle(h) }

// list reads every entry of h into s.ls. Sizes and mtimes are always filled
// because the listing returns them for free.
func (s *session) list(h handle, _ Detail, _ uint32) error {
	if len(s.buf) < listBufSize {
		s.buf = make([]byte, listBufSize)
	}
	buf := s.buf
	restart := uintptr(1)
	for {
		var iosb windows.IO_STATUS_BLOCK
		r, _, _ := syscall.SyscallN(procNtQueryDirectoryFile.Addr(),
			uintptr(h), 0, 0, 0, uintptr(unsafe.Pointer(&iosb)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
			fileDirectoryInformation, 0, 0, restart)
		restart = 0
		switch status := windows.NTStatus(r); status {
		case windows.STATUS_SUCCESS:
		case windows.STATUS_NO_MORE_FILES, statusNoSuchFile:
			return nil
		default:
			if int32(status) < 0 {
				return ntErr(status)
			}
		}
		n := int(iosb.Information)
		if n == 0 {
			return nil
		}
		if n > len(buf) {
			return windows.ERROR_INVALID_DATA
		}
		for off := 0; ; {
			if off+fdiFileName > n {
				return windows.ERROR_INVALID_DATA
			}
			rec := buf[off:n]
			next := int(binary.LittleEndian.Uint32(rec[fdiNextEntryOffset:]))
			nameLen := int(binary.LittleEndian.Uint32(rec[fdiFileNameLength:]))
			if nameLen%2 != 0 || fdiFileName+nameLen > len(rec) {
				return windows.ERROR_INVALID_DATA
			}
			wname := unsafe.Slice((*uint16)(unsafe.Pointer(&rec[fdiFileName])), nameLen/2)
			if !isDotOrDotDotUTF16(wname) && nameLen > 0 {
				attrs := binary.LittleEndian.Uint32(rec[fdiFileAttributes:])
				typ, size := File, int64(binary.LittleEndian.Uint64(rec[fdiEndOfFile:]))
				switch {
				case attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
					typ, size = Symlink, -1
				case attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
					typ, size = Dir, -1
				}
				mtime := (int64(binary.LittleEndian.Uint64(rec[fdiLastWriteTime:])) - unixEpochTicks) * 100
				ls := &s.ls
				ls.Entries = append(ls.Entries, Record{off: uint32(len(ls.names)), Type: typ, Size: size, Mtime: mtime})
				ls.names = appendWTF8(ls.names, wname)
				r := &ls.Entries[len(ls.Entries)-1]
				r.nlen = uint32(len(ls.names)) - r.off
			}
			if next == 0 {
				break
			}
			if next < fdiFileName || off+next > n {
				return windows.ERROR_INVALID_DATA
			}
			off += next
		}
	}
}

func isDotOrDotDotUTF16(name []uint16) bool {
	return len(name) > 0 && len(name) <= 2 && name[0] == '.' && (len(name) == 1 || name[1] == '.')
}

func (s *session) OpenFile(dir Handle, name string) (Handle, FileInfo, error) {
	var h handle
	var err error
	if dir.ok {
		h, err = s.ntOpen(dir.native(), name, fileAccess, windows.FILE_NON_DIRECTORY_FILE)
	} else {
		h, err = s.createFile(name, fileAccess, 0)
	}
	if err != nil {
		return Handle{}, FileInfo{}, err
	}
	st, typ, size, err := statHandle(h)
	if err == nil && typ != File {
		err = ErrNotRegular
	}
	if err != nil {
		windows.CloseHandle(h)
		return Handle{}, FileInfo{}, err
	}
	return Handle{h: uintptr(h), size: size, ok: true}, FileInfo{Type: File, Size: size, Stamp: st}, nil
}

// Read passes each read's offset in an OVERLAPPED, which on a synchronous
// handle makes it positional; at end of file such a read fails with
// ERROR_HANDLE_EOF instead of returning 0 bytes.
func (s *session) Read(h Handle, buf []byte, limit int64) ([]byte, error) {
	return readInto(buf, h.size, limit, func(p []byte, off int64) (int, error) {
		var n uint32
		o := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
		err := windows.ReadFile(h.native(), p, &n, &o)
		if err == windows.ERROR_HANDLE_EOF {
			err = nil
		}
		return int(n), err
	})
}

// Map is unsupported: mapping was only measured to beat reading on darwin,
// so Windows always reads.
func (*session) Map(Handle, int64) ([]byte, error) { return nil, errors.ErrUnsupported }

func (*session) Unmap([]byte) {}
