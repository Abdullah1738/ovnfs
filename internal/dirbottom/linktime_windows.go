//go:build windows

package dirbottom

import (
	"time"

	"golang.org/x/sys/windows"
)

// linkTime restores the reparse point's own timestamps without following its target.
func linkTime(path string, mtime time.Time) error {
	name, err := windows.UTF16PtrFromString(path)
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
