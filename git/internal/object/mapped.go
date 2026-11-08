package object

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
)

// mappedFile is a read-only file, memory-mapped when the platform allows so
// lookups in large pack indexes touch only the pages they need, and read
// through ReadAt otherwise. Views returned by slice must not outlive close.
type mappedFile struct {
	data []byte   // the whole file, when mapped
	file *os.File // the open file, when not mapped
	size int64
}

// mapFiles enables memory mapping; tests turn it off to exercise ReadAt.
var mapFiles = true

// MapFile returns the contents of the file at path read-only, memory-mapped
// where the platform allows so large files cost only the pages touched, and
// read into memory otherwise. The data must not be modified, nor used after
// unmap is called.
func MapFile(path string) (data []byte, unmap func() error, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if size := fi.Size(); mapFiles && size > 0 && size <= math.MaxInt {
		if data, err := mmap(f, int(size)); err == nil {
			return data, func() error { return munmap(data) }, nil
		}
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	return data, func() error { return nil }, nil
}

func openMapped(path string) (*mappedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := fi.Size()
	if mapFiles && size > 0 && size <= math.MaxInt {
		if data, err := mmap(f, int(size)); err == nil {
			// The mapping outlives the descriptor; closing it now keeps
			// descriptor use flat however many packs a repository has.
			f.Close()
			return &mappedFile{data: data, size: size}, nil
		}
	}
	return &mappedFile{file: f, size: size}, nil
}

// ReadAt implements io.ReaderAt.
func (m *mappedFile) ReadAt(p []byte, off int64) (int, error) {
	if m.data == nil {
		return m.file.ReadAt(p, off)
	}
	if off < 0 || off >= m.size {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// slice returns the n bytes at off: a view of the mapping, or a copy read
// from the file.
func (m *mappedFile) slice(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > m.size {
		return nil, fmt.Errorf("read of %d bytes at %d beyond end of %d-byte file", n, off, m.size)
	}
	if m.data != nil {
		return m.data[off : off+n], nil
	}
	b := make([]byte, n)
	if _, err := m.file.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

// reader returns a byte reader over the file from off to end.
func (m *mappedFile) reader(off int64) io.Reader {
	if m.data != nil {
		return bytes.NewReader(m.data[off:])
	}
	return bufio.NewReaderSize(io.NewSectionReader(m.file, off, m.size-off), 32<<10)
}

func (m *mappedFile) close() error {
	if m.data != nil {
		data := m.data
		m.data = nil
		return munmap(data)
	}
	return m.file.Close()
}
