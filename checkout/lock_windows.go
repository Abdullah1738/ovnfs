//go:build windows

package checkout

import (
	"os"
	"syscall"
	"unsafe"
)

// lockState holds an exclusive nonblocking byte-range lock until the file closes.
func tryStateLock(f *os.File) (bool, error) {
	lock := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	var overlapped syscall.Overlapped
	result, _, callErr := lock.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if result == 0 {
		if callErr == syscall.Errno(33) {
			return false, nil
		}
		return false, callErr
	}
	return true, nil
}
