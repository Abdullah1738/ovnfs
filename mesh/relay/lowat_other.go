//go:build !linux && !darwin

package relay

import "syscall"

// notsentLowat does nothing where TCP_NOTSENT_LOWAT is not available.
func notsentLowat(_, _ string, _ syscall.RawConn) error { return nil }
