package nfs

import (
	"errors"
	"os"
	"syscall"
)

// status is an nfsstat3 or nfsstat4 value. The two protocols share the
// number of every error both define; NFSv4-only statuses are in v4*.go.
type status uint32

// Statuses NFSv3 (RFC 1813) and NFSv4.0 (RFC 7530) share.
const (
	stOK          status = 0
	stPerm        status = 1
	stNoEnt       status = 2
	stIO          status = 5
	stNXIO        status = 6
	stAccess      status = 13
	stExist       status = 17
	stXDev        status = 18
	stNotDir      status = 20
	stIsDir       status = 21
	stInval       status = 22
	stFBig        status = 27
	stNoSpc       status = 28
	stROFS        status = 30
	stMLink       status = 31
	stNameTooLong status = 63
	stNotEmpty    status = 66
	stDQuot       status = 69
	stStale       status = 70
	stBadHandle   status = 10001
	stBadCookie   status = 10003
	stNotSupp     status = 10004
	stTooSmall    status = 10005
	stServerFault status = 10006
	stBadType     status = 10007
	// stDelay is NFS3ERR_JUKEBOX and NFS4ERR_DELAY: try again later.
	stDelay status = 10008
)

// statusOf maps a backend error to the status that keeps its errno, so
// clients see EXDEV (and copy across checkouts), ENOTEMPTY, EPERM and EACCES
// as such rather than as I/O errors. Errors without a closer status are stIO.
func statusOf(err error) status {
	// Order matters: os.ErrExist matches ENOTEMPTY and os.ErrPermission
	// matches EPERM.
	switch {
	case err == nil:
		return stOK
	case errors.Is(err, syscall.EXDEV):
		return stXDev
	case errors.Is(err, syscall.ENOTEMPTY):
		return stNotEmpty
	case errors.Is(err, os.ErrExist):
		return stExist
	case errors.Is(err, syscall.EPERM):
		return stPerm
	case errors.Is(err, os.ErrPermission):
		return stAccess
	case errors.Is(err, syscall.EROFS):
		return stROFS
	case errors.Is(err, os.ErrNotExist):
		return stNoEnt
	case errors.Is(err, syscall.ENOTDIR):
		return stNotDir
	case errors.Is(err, syscall.EISDIR):
		return stIsDir
	case errors.Is(err, syscall.ENAMETOOLONG):
		return stNameTooLong
	case errors.Is(err, syscall.EMLINK):
		return stMLink
	case errors.Is(err, syscall.ENOSPC):
		return stNoSpc
	case errors.Is(err, syscall.EDQUOT):
		return stDQuot
	case errors.Is(err, syscall.EFBIG):
		return stFBig
	case errors.Is(err, syscall.EINVAL):
		return stInval
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP):
		return stNotSupp
	case errors.Is(err, syscall.ESTALE):
		return stStale
	}
	return stIO
}
