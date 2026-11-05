package admin

import (
	"errors"
	"fmt"
	"net"
	"os"
)

// Listen creates a private Unix socket at path, removing only a demonstrably
// stale socket. It fails when path is not a socket, when a server already
// answers there, or when the socket is replaced while its liveness is checked.
func Listen(path string) (net.Listener, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		conn, dialErr := net.Dial("unix", path)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("socket already listening: %s", path)
		}
		if !refused(dialErr) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, dialErr
		}
		current, statErr := os.Lstat(path)
		if statErr == nil {
			if !os.SameFile(info, current) {
				return nil, fmt.Errorf("socket %s changed while checking liveness", path)
			}
			if err := os.Remove(path); err != nil {
				return nil, err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := restrict(path); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}
