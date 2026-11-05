//go:build darwin

package remotefs

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// checkPeer authenticates the Unix socket's effective peer uid.
func checkPeer(conn net.Conn, allowed []uint32) error {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("remotefs requires a Unix connection")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return fmt.Errorf("remotefs peer credentials: %w", err)
	}
	var credErr error
	err = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			credErr = fmt.Errorf("remotefs peer credentials: %w", err)
			return
		}
		credErr = checkUID(cred.Uid, allowed)
	})
	if err != nil {
		return fmt.Errorf("remotefs peer credentials: %w", err)
	}
	return credErr
}
