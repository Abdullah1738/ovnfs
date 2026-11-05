package fsys

import (
	"path/filepath"
	"sync"
)

// Native is the host filesystem. Directory handles are real descriptors
// (Windows: HANDLEs) and every operation is a direct syscall; see the
// native_*.go files for the per-platform fast paths.
var Native FS = &nativeFS{}

type nativeFS struct {
	stable sync.Map // device → bool, for StableStamps
}

func (*nativeFS) Abs(root string) (string, error) { return filepath.Abs(root) }

func (n *nativeFS) NewSession() Session { return &session{fs: n} }

func (*nativeFS) Separator() byte { return pathSep }

func (*nativeFS) Workers() int { return defaultWorkers }

// session is per-goroutine scratch: buffers are reused across calls so
// steady-state listing and reading allocate nothing.
type session struct {
	fs    *nativeFS
	ls    Listing
	buf   []byte   // bulk listing syscall buffer
	cname []byte   // NUL-terminated name for *at syscalls
	wname []uint16 // Windows: UTF-16 name for NtCreateFile
}

// cstr returns name NUL-terminated in s.cname.
func (s *session) cstr(name string) []byte {
	s.cname = append(append(s.cname[:0], name...), 0)
	return s.cname
}

func wrap(h handle) Handle { return Handle{h: uintptr(h), ok: true} }

func (h Handle) native() handle { return handle(h.h) }

func (s *session) OpenDir(path string) (Handle, error) {
	h, err := s.openDir(path)
	if err != nil {
		return Handle{}, err
	}
	return wrap(h), nil
}

func (s *session) OpenChild(parent Handle, name string) (Handle, error) {
	h, err := s.openChild(parent.native(), name)
	if err != nil {
		return Handle{}, err
	}
	return wrap(h), nil
}

func (s *session) Close(h Handle) {
	if h.ok {
		closeHandle(h.native())
	}
}

func (s *session) List(h Handle, detail Detail, children uint32) (*Listing, error) {
	s.ls.reset()
	if err := s.list(h.native(), detail, children); err != nil {
		return nil, err
	}
	return &s.ls, nil
}

func (s *session) StatDir(h Handle) (Stamp, error) {
	st, _, _, err := statHandle(h.native())
	return st, err
}

func (s *session) StableStamps(h Handle, st Stamp) bool {
	if !memoStableByDev {
		return fineTimestamps(h.native())
	}
	if ok, found := s.fs.stable.Load(st.Dev); found {
		return ok.(bool)
	}
	ok := fineTimestamps(h.native())
	s.fs.stable.Store(st.Dev, ok)
	return ok
}

// grow returns buf with capacity for at least n bytes, keeping its contents.
func grow(buf []byte, n int) []byte {
	if cap(buf) >= n {
		return buf
	}
	return append(buf[:cap(buf)], make([]byte, n-cap(buf))...)[:len(buf)]
}

// readInto fills buf from read (of p at file offset off) until EOF or
// limit bytes, given size from fstat. A read that returns exactly the
// remaining size is taken as end of file, saving the confirming zero-length
// read per file; files reporting size 0 (pseudo files) and short reads keep
// reading until EOF.
func readInto(buf []byte, size, limit int64, read func(p []byte, off int64) (int, error)) ([]byte, error) {
	buf = buf[:0]
	want := int64(4096) // size 0: empty, or a pseudo file of unknown size
	if size > 0 {
		want = min(size, limit)
	}
	buf = grow(buf, int(min(want, limit)))
	for int64(len(buf)) < limit {
		if len(buf) == cap(buf) {
			buf = grow(buf, int(min(limit, int64(2*cap(buf)+4096))))
		}
		n, err := read(buf[len(buf):min(cap(buf), int(limit))], int64(len(buf)))
		if err != nil {
			return buf, err
		}
		if n == 0 {
			break
		}
		buf = buf[:len(buf)+n]
		if size > 0 && int64(len(buf)) == size {
			break
		}
	}
	return buf, nil
}
