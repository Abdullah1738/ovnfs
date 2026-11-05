//go:build !darwin && !linux && !windows

package admin

import (
	"errors"
	"net"
)

// CheckPeer refuses platforms without a supported peer credential mechanism.
func CheckPeer(conn net.Conn) error {
	return errors.New("admin peer credentials unsupported on this platform")
}
