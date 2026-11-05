//go:build !windows

package remotefs

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// Listen creates the share socket at path with mode 0600, first removing a
// stale socket left by a dead server; a live socket or a non-socket is an error.
func Listen(path string) (net.Listener, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("remotefs: %s exists and is not a socket", path)
		}
		conn, dialErr := net.Dial("unix", path)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("remotefs: socket %s is already served", path)
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, fmt.Errorf("remotefs: probe socket %s: %w", path, dialErr)
		}
		current, statErr := os.Lstat(path)
		switch {
		case statErr == nil:
			if !os.SameFile(info, current) {
				return nil, fmt.Errorf("remotefs: socket %s changed while checking liveness", path)
			}
			if err := os.Remove(path); err != nil {
				return nil, fmt.Errorf("remotefs: remove stale socket: %w", err)
			}
		case !errors.Is(statErr, os.ErrNotExist):
			return nil, fmt.Errorf("remotefs: stat socket: %w", statErr)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("remotefs: stat socket: %w", err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("remotefs: listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("remotefs: chmod socket: %w", err)
	}
	return l, nil
}
