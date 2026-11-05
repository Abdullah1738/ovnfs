//go:build !windows

package checkout

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lockState prevents another server from concurrently rewriting the same manifest.
func lockState(state string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(state, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, coded(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, failure("exists", "state directory is already open: %s", state)
		}
		return nil, coded(err)
	}
	return f, nil
}
