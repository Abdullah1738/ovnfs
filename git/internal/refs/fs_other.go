//go:build !windows

package refs

import "os"

// readFile reads the file at path.
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// createExclusive creates path for reading and writing, failing with an
// error wrapping fs.ErrExist if it exists.
func createExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
}

// replaceFile atomically renames oldpath over newpath.
func replaceFile(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
