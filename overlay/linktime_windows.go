//go:build windows

package overlay

import (
	"time"

	"golang.org/x/sys/windows"
)

// linkTime restores timestamps on the reparse point itself, without following it.
func linkTime(p string, mtime time.Time) error {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	t := windows.NsecToFiletime(mtime.UnixNano())
	return windows.SetFileTime(h, nil, &t, &t)
}
