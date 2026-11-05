//go:build darwin

package admin

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// CheckPeer rejects a Unix socket peer whose effective uid differs from this process's.
func CheckPeer(conn net.Conn) error {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("admin requires a Unix connection")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var credentialErr error
	err = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			credentialErr = err
			return
		}
		credentialErr = checkUID(cred.Uid, uint32(os.Geteuid()))
	})
	if err != nil {
		return err
	}
	return credentialErr
}
