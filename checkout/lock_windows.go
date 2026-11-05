//go:build windows

package checkout

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// lockState holds an exclusive nonblocking byte-range lock until the file closes.
func lockState(state string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(state, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, coded(err)
	}
	lock := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	var overlapped syscall.Overlapped
	result, _, callErr := lock.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if result == 0 {
		f.Close()
		if callErr == syscall.Errno(33) {
			return nil, failure("exists", "state directory is already open: %s", state)
		}
		return nil, coded(callErr)
	}
	return f, nil
}
