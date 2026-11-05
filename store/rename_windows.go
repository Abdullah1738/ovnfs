package store

import (
	"errors"

	"golang.org/x/sys/windows"
)

// renameNoReplace renames old to new unless new exists, reporting whether it did.
func renameNoReplace(old, new string) (bool, error) {
	from, err := windows.UTF16PtrFromString(old)
	if err != nil {
		return false, err
	}
	to, err := windows.UTF16PtrFromString(new)
	if err != nil {
		return false, err
	}
	err = windows.MoveFileEx(from, to, 0)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return true, nil
	}
	return false, err
}
