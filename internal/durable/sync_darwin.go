package durable

import (
	"os"

	"golang.org/x/sys/unix"
)

// Sync flushes f with fsync(2) rather than F_FULLFSYNC; see the package doc.
func Sync(f *os.File) error {
	return unix.Fsync(int(f.Fd()))
}
