//go:build !windows

package checkout

import (
	"errors"
	"os"
	"syscall"
)

// lockState prevents another server from concurrently rewriting the same manifest.
func tryStateLock(f *os.File) (bool, error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
