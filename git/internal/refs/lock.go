package refs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// lockFile is git's lock protocol: "<target>.lock" created exclusively
// excludes every other git-compatible writer of target; renaming it over
// target publishes the new content atomically.
type lockFile struct {
	path, target string
	f            *os.File
	done         bool
}

// lockOptions tunes createLock.
type lockOptions struct {
	// mkdirs creates missing parent directories, for a ref being created.
	mkdirs bool
}

// createLock takes the lock for target; an existing lock file yields an
// error wrapping ErrLocked.
func createLock(target string, opts lockOptions) (*lockFile, error) {
	path := target + ".lock"
	if opts.mkdirs {
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("refs: unable to create %s: %w", path, ErrLocked)
		}
		return nil, err
	}
	return &lockFile{path: path, target: target, f: f}, nil
}

// write stores data as the lock's content and closes the file, which must
// happen before renaming on Windows.
func (l *lockFile) write(data []byte) error {
	_, err := l.f.Write(data)
	if cerr := l.close(); err == nil {
		err = cerr
	}
	return err
}

func (l *lockFile) close() error {
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// commit renames the lock over its target, releasing the lock.
func (l *lockFile) commit() error {
	if err := l.close(); err != nil {
		return err
	}
	if err := os.Rename(l.path, l.target); err != nil {
		return err
	}
	l.done = true
	return nil
}

// rollback releases the lock without touching the target. It is a no-op
// after commit, so it can be deferred.
func (l *lockFile) rollback() {
	if l.done {
		return
	}
	l.close()
	os.Remove(l.path)
	l.done = true
}
