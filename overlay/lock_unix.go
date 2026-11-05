//go:build !windows

package overlay

import "golang.org/x/sys/unix"

// Lock acquires an exclusive advisory lock on the whole backing file.
func (f *file) Lock() error { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }

// Unlock releases the advisory lock on the backing file.
func (f *file) Unlock() error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
