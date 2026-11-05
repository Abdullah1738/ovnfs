package ntinfo

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// NTSTATUS values (MS-ERREF) returned by Status.
const (
	statusSuccess               uint32 = 0x00000000
	statusStoppedOnSymlink      uint32 = 0x8000002d
	statusInvalidParameter      uint32 = 0xc000000d
	statusEndOfFile             uint32 = 0xc0000011
	statusAccessDenied          uint32 = 0xc0000022
	statusObjectNameInvalid     uint32 = 0xc0000033
	statusObjectNameNotFound    uint32 = 0xc0000034
	statusObjectNameCollision   uint32 = 0xc0000035
	statusSharingViolation      uint32 = 0xc0000043
	statusDiskFull              uint32 = 0xc000007f
	statusInsufficientResources uint32 = 0xc000009a
	statusFileIsADirectory      uint32 = 0xc00000ba
	statusNotSupported          uint32 = 0xc00000bb
	statusNotSameDevice         uint32 = 0xc00000d4
	statusInternalError         uint32 = 0xc00000e5
	statusDirectoryNotEmpty     uint32 = 0xc0000101
	statusNotADirectory         uint32 = 0xc0000103
	statusCancelled             uint32 = 0xc0000120
	statusFileClosed            uint32 = 0xc0000128
	statusTooManyLinks          uint32 = 0xc0000265
)

// Status maps a filesystem error from the export to the NTSTATUS a Windows
// client sees (STATUS_SUCCESS for nil, STATUS_INTERNAL_ERROR when unrecognized).
func Status(err error) uint32 {
	if err == nil {
		return statusSuccess
	}
	switch {
	case errors.Is(err, io.EOF):
		return statusEndOfFile
	case errors.Is(err, os.ErrClosed), errors.Is(err, syscall.EBADF):
		return statusFileClosed
	case errors.Is(err, syscall.ENOTDIR):
		return statusNotADirectory
	case errors.Is(err, syscall.EISDIR):
		return statusFileIsADirectory
	case errors.Is(err, syscall.ENOTEMPTY):
		return statusDirectoryNotEmpty
	case errors.Is(err, os.ErrNotExist):
		return statusObjectNameNotFound
	case errors.Is(err, os.ErrExist):
		return statusObjectNameCollision
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EPERM):
		return statusAccessDenied
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return statusDiskFull
	case errors.Is(err, syscall.EXDEV):
		return statusNotSameDevice
	case errors.Is(err, syscall.ELOOP):
		return statusStoppedOnSymlink
	case errors.Is(err, syscall.ENAMETOOLONG):
		return statusObjectNameInvalid
	case errors.Is(err, syscall.EMLINK):
		return statusTooManyLinks
	case errors.Is(err, syscall.EINVAL):
		return statusInvalidParameter
	case errors.Is(err, syscall.ENOSYS), errors.Is(err, syscall.ENOTSUP):
		return statusNotSupported
	case errors.Is(err, syscall.EBUSY), errors.Is(err, syscall.ETXTBSY):
		return statusSharingViolation
	case errors.Is(err, syscall.ENOMEM), errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return statusInsufficientResources
	case errors.Is(err, syscall.ECANCELED):
		return statusCancelled
	default:
		return statusInternalError
	}
}
