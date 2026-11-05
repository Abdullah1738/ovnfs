package overlay

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"syscall"

	"github.com/go-git/go-billy/v5"
)

// Content is an open read-only regular file served by a [Bottom]: a host file,
// or bytes already in memory such as a cached store blob ([ByteContent]).
type Content interface {
	io.ReadSeekCloser
	io.ReaderAt
}

// ByteContent serves data as Content without a host file. data must not change
// afterwards; any number of readers may share it.
func ByteContent(data []byte) Content { return byteContent{bytes.NewReader(data)} }

// byteContent closes as a no-op because it holds no host resources.
type byteContent struct{ *bytes.Reader }

// Close releases nothing; the shared bytes stay owned by their cache.
func (byteContent) Close() error { return nil }

// contentFile exposes non-host bottom Content as a read-only billy file.
type contentFile struct {
	Content
	name string
}

// Name returns the overlay-relative name.
func (c *contentFile) Name() string { return c.name }

// Write fails like a write to a read-only descriptor.
func (c *contentFile) Write([]byte) (int, error) { return 0, c.badDescriptor("write") }

// Truncate fails like a truncate of a read-only descriptor.
func (c *contentFile) Truncate(int64) error { return c.badDescriptor("truncate") }

// Lock is a no-op: in-memory content has no host file to lock and never changes.
func (c *contentFile) Lock() error { return nil }

// Unlock is a no-op, matching Lock.
func (c *contentFile) Unlock() error { return nil }

// badDescriptor reports a mutation through a read-only handle.
func (c *contentFile) badDescriptor(op string) error {
	return &fs.PathError{Op: op, Path: c.name, Err: syscall.EBADF}
}

// wrapContent keeps host files on the ordinary descriptor path so locks and
// descriptor-level calls still reach the OS.
func wrapContent(c Content, name string) billy.File {
	if f, ok := c.(*os.File); ok {
		return &file{File: f, name: name}
	}
	return &contentFile{Content: c, name: name}
}
