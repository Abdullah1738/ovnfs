//go:build linux || darwin

package relay

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// notsentLowat caps the unsent bytes the kernel takes for a link's socket
// (TCP_NOTSENT_LOWAT, 32 KiB) so a backlog stays in the link's queue, where
// sojourn bounds it. A socket that refuses it still works.
func notsentLowat(_, _ string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, 32<<10)
	})
}
