//go:build !darwin && !linux

package remotefs

import (
	"errors"
	"net"
)

// checkPeer refuses platforms without a supported peer credential mechanism.
func checkPeer(net.Conn, []uint32) error {
	return errors.New("remotefs peer credentials unsupported on this platform")
}
