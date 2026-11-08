//go:build !windows && !darwin

package hostfile

import "golang.org/x/sys/unix"

// utimeOmit tells utimensat to leave a time unchanged.
const utimeOmit = unix.UTIME_OMIT
