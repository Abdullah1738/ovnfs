//go:build !unix && !windows

package store

import (
	"errors"
	"os"
)

// openRegular opens path and checks it is a regular file; these platforms have no
// no-follow open. It returns the file's size; the device is unknown here (0).
func openRegular(path string) (*os.File, int64, uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, 0, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	if info, err = f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, 0, 0, errors.New("not a regular file")
	}
	return f, info.Size(), 0, nil
}

// syncDir makes a rename in dir durable where the platform allows it.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		f.Sync()
		f.Close()
	}
}
