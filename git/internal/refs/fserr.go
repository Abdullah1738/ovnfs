package refs

import (
	"errors"
	"syscall"
)

// isNotDir reports ENOTDIR: a path component that should be a directory is a
// file, as when reading refs/heads/a/b while refs/heads/a is a loose ref. For
// a lookup that means the ref does not exist.
func isNotDir(err error) bool { return errors.Is(err, syscall.ENOTDIR) }
