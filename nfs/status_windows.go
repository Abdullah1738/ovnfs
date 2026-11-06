package nfs

import (
	"errors"
	"syscall"
)

// notEmpty reports whether err says a directory is not empty. Windows
// returns ERROR_DIR_NOT_EMPTY, which os.ErrExist matches, not ENOTEMPTY.
func notEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.ERROR_DIR_NOT_EMPTY)
}
