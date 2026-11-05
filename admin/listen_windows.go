//go:build windows

package admin

import (
	"errors"

	"golang.org/x/sys/windows"
)

// refused reports a dial error meaning no server is behind the socket file.
func refused(err error) bool { return errors.Is(err, windows.WSAECONNREFUSED) }

// restrict leaves access to the socket file's ACL, inherited from its
// directory (mode bits do not apply on Windows); CheckPeer enforces the user.
func restrict(string) error { return nil }
