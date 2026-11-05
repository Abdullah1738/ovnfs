//go:build windows

package remotefs

import (
	"errors"
	"net"
)

// Listen reports that share sockets are unavailable on Windows: the VMM's
// Windows device dials a named pipe, which this package does not serve.
func Listen(string) (net.Listener, error) {
	return nil, errors.New("remotefs: share sockets are unsupported on Windows")
}
