package store

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// openRegular opens path only if it is a regular file, opening a reparse
// point itself rather than its target so a swapped-in link is refused. It
// returns the file's size and volume serial from one handle query.
func openRegular(path string) (*os.File, int64, uint64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, 0, 0, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, 0, 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, 0, 0, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		windows.CloseHandle(h)
		return nil, 0, 0, errors.New("not a regular file")
	}
	size := int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
	return os.NewFile(uintptr(h), path), size, uint64(info.VolumeSerialNumber), nil
}

// syncDir is unnecessary on Windows, whose directory handles cannot be flushed.
func syncDir(string) {}
