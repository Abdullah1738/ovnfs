package export

import (
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
)

// rootInfo describes the virtual directory containing all live checkouts.
type rootInfo struct {
	mtime time.Time
}

// Name returns the export root's display name.
func (rootInfo) Name() string { return "/" }

// Size returns zero because the virtual root stores no file data.
func (rootInfo) Size() int64 { return 0 }

// Mode returns the immutable export-root directory permissions.
func (rootInfo) Mode() os.FileMode { return os.ModeDir | 0o555 }

// ModTime tracks checkout creation and removal through the registry.
func (i rootInfo) ModTime() time.Time { return i.mtime }

// IsDir identifies the export root as a directory.
func (rootInfo) IsDir() bool { return true }

// Sys returns no host-specific stat for the virtual root.
func (rootInfo) Sys() any { return nil }

// checkoutRootInfo retains real root attributes while exposing the checkout's export name.
type checkoutRootInfo struct {
	os.FileInfo
	name string
}

// Name returns the immutable checkout folder name.
func (i checkoutRootInfo) Name() string { return i.name }

// rootFile is a read-only directory handle for the virtual export root.
type rootFile struct {
	mu     sync.Mutex
	router *Router
	name   string
	closed bool
}

// rootFileInterface checks the virtual directory supports the billy file contract.
var _ billy.File = (*rootFile)(nil)

// Name returns the path supplied when opening the virtual root.
func (f *rootFile) Name() string { return f.name }

// failure reports closed handles before an operation's directory-specific error.
func (f *rootFile) failure(op string, err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		err = os.ErrClosed
	}
	return &os.PathError{Op: op, Path: f.name, Err: err}
}

// Read rejects byte reads of the virtual directory.
func (f *rootFile) Read([]byte) (int, error) { return 0, f.failure("read", syscall.EISDIR) }

// ReadAt rejects positioned byte reads of the virtual directory.
func (f *rootFile) ReadAt([]byte, int64) (int, error) {
	return 0, f.failure("readat", syscall.EISDIR)
}

// Write rejects changes to the virtual directory.
func (f *rootFile) Write([]byte) (int, error) {
	return 0, f.failure("write", os.ErrPermission)
}

// Seek rejects byte positioning in the virtual directory.
func (f *rootFile) Seek(int64, int) (int64, error) {
	return 0, f.failure("seek", syscall.EISDIR)
}

// Truncate rejects resizing the virtual directory.
func (f *rootFile) Truncate(int64) error { return f.failure("truncate", os.ErrPermission) }

// Lock reports that the virtual directory has no host file to lock.
func (f *rootFile) Lock() error { return f.failure("lock", billy.ErrNotSupported) }

// Unlock reports that the virtual directory has no host file to unlock.
func (f *rootFile) Unlock() error { return f.failure("unlock", billy.ErrNotSupported) }

// Stat returns live export-root attributes for an open directory handle.
func (f *rootFile) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, &os.PathError{Op: "stat", Path: f.name, Err: os.ErrClosed}
	}
	return rootInfo{mtime: f.router.reg.RootTime()}, nil
}

// Close invalidates this virtual directory handle.
func (f *rootFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return &os.PathError{Op: "close", Path: f.name, Err: os.ErrClosed}
	}
	f.closed = true
	return nil
}
