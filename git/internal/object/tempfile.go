package object

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// createTemp creates a new file prefix+random in dir, open for reading and
// writing, with permissions perm less the umask. Unlike os.CreateTemp it
// takes the final permissions up front, so read-only objects and packs need
// no chmod that would have to guess the umask.
func createTemp(dir, prefix string, perm fs.FileMode) (*os.File, error) {
	for {
		name := filepath.Join(dir, prefix+rand.Text()[:12])
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
}

// publish moves the closed temporary file tmp to dst unless dst exists, in
// which case tmp is discarded: objects and packs are named by their content,
// so an existing file already holds the same bytes and is never replaced.
func publish(tmp, dst string) error {
	if runtime.GOOS != "windows" {
		// A hard link fails instead of replacing an existing dst.
		err := os.Link(tmp, dst)
		if err == nil || errors.Is(err, fs.ErrExist) {
			os.Remove(tmp)
			return nil
		}
		// Filesystems without hard links fall back to rename.
	}
	if _, err := os.Lstat(dst); err == nil {
		removeTemp(tmp)
		return nil
	}
	return os.Rename(tmp, dst)
}

// removeTemp deletes a temporary file, which may be read-only: Windows
// refuses to delete read-only files.
func removeTemp(path string) {
	if runtime.GOOS == "windows" {
		os.Chmod(path, 0o644)
	}
	os.Remove(path)
}
