//go:build windows

package overlay

import "golang.org/x/sys/windows"

// Lock acquires a blocking exclusive lock over the entire backing file.
func (f *file) Lock() error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, ^uint32(0), ^uint32(0), &overlapped)
}

// Unlock releases the whole-file lock acquired by Lock.
func (f *file) Unlock() error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, ^uint32(0), ^uint32(0), &overlapped)
}
