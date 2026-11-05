package remotefs

import (
	"errors"
	"io/fs"
	"syscall"

	"github.com/go-git/go-billy/v5"
)

// errnoCodes maps the errnos the export returns to wire codes. It is filled by
// assignment because some errnos alias on some platforms (Linux ENOTSUP == EOPNOTSUPP).
var errnoCodes = func() map[syscall.Errno]string {
	m := make(map[syscall.Errno]string)
	for _, c := range []struct {
		errno syscall.Errno
		code  string
	}{
		{syscall.ENOENT, CodeNotFound},
		{syscall.EACCES, CodeAccess},
		{syscall.EPERM, CodePerm},
		{syscall.EEXIST, CodeExists},
		{syscall.ENOTEMPTY, CodeNotEmpty},
		{syscall.EISDIR, CodeIsDir},
		{syscall.ENOTDIR, CodeNotDir},
		{syscall.EXDEV, CodeXdev},
		{syscall.EROFS, CodeRofs},
		{syscall.EFBIG, CodeTooBig},
		{syscall.ENOSPC, CodeNoSpace},
		{syscall.EDQUOT, CodeNoSpace},
		{syscall.ESTALE, CodeStale},
		{syscall.EINVAL, CodeBadRequest},
		{syscall.ENAMETOOLONG, CodeBadRequest},
		{syscall.ENOSYS, CodeUnsupported},
		{syscall.ENOTSUP, CodeUnsupported},
		{syscall.EOPNOTSUPP, CodeUnsupported},
	} {
		m[c.errno] = c.code
	}
	return m
}()

// wireError converts a filesystem error into the ERR reply the VMM maps back to an errno.
func wireError(err error) *Error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	code := CodeIO
	var errno syscall.Errno
	switch {
	case errors.As(err, &errno) && errnoCodes[errno] != "":
		code = errnoCodes[errno]
	case errors.Is(err, fs.ErrNotExist):
		code = CodeNotFound
	case errors.Is(err, fs.ErrExist):
		code = CodeExists
	case errors.Is(err, fs.ErrPermission):
		code = CodePerm
	case errors.Is(err, fs.ErrInvalid):
		code = CodeBadRequest
	case errors.Is(err, billy.ErrNotSupported):
		code = CodeUnsupported
	case errors.Is(err, billy.ErrReadOnly):
		code = CodeRofs
	}
	return &Error{Code: code, Msg: err.Error()}
}

// errorf returns a wire error with the given code.
func errorf(code, msg string) *Error { return &Error{Code: code, Msg: msg} }
