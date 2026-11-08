//go:build windows

package hostfile

import (
	"io/fs"
	"time"

	"golang.org/x/sys/windows"
)

// Lchtimes sets the access and modification times of p itself, without
// following a reparse point; a zero time leaves that time unchanged, as for
// os.Chtimes.
func Lchtimes(p string, atime, mtime time.Time) error {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return &fs.PathError{Op: "chtimes", Path: p, Err: err}
	}
	defer windows.CloseHandle(h)
	if err := windows.SetFileTime(h, nil, filetime(atime), filetime(mtime)); err != nil {
		return &fs.PathError{Op: "chtimes", Path: p, Err: err}
	}
	return nil
}

// filetime converts t for SetFileTime; nil, for the zero time, keeps the time.
func filetime(t time.Time) *windows.Filetime {
	if t.IsZero() {
		return nil
	}
	ft := windows.NsecToFiletime(t.UnixNano())
	return &ft
}
