//go:build windows

package refs

import (
	"bytes"
	"io/fs"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows refuses to rename over a file while any handle on it lacks
// FILE_SHARE_DELETE, and a classic rename refuses even then. Git for Windows
// (compat/mingw.c) therefore opens files sharing delete access and renames
// with POSIX semantics, which replace a target that is still open; doing the
// same keeps concurrent readers, ours or git's, from failing lock commits.

// readFile reads the file at path without keeping writers from renaming
// over it.
func readFile(path string) ([]byte, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(make([]byte, 0, info.Size()+bytes.MinRead))
	_, err = buf.ReadFrom(f)
	return buf.Bytes(), err
}

var rtlGetLastNtStatus = windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlGetLastNtStatus")

// createExclusive creates path for reading and writing, failing with an
// error wrapping fs.ErrExist if it exists. Concurrent writers releasing a
// lock on the same name can make creation fail with ERROR_ACCESS_DENIED for
// a moment, so that is retried as renames are.
func createExclusive(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err == nil {
		// Loaded up front: loading it after the failure could replace the
		// status.
		err = rtlGetLastNtStatus.Find()
	}
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	var h windows.Handle
	err = retryInUse(func() (err error) {
		h, err = createNew(p)
		return err
	})
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// createNew creates the file at p, failing with ERROR_FILE_EXISTS if it
// exists. As in Git for Windows' mingw_open, a file still being deleted
// exists: creating it fails with ERROR_ACCESS_DENIED, and the thread's last
// NTSTATUS is STATUS_DELETE_PENDING.
func createNew(p *uint16) (windows.Handle, error) {
	// The status must be read on the thread that made the call.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err == windows.ERROR_ACCESS_DENIED {
		if st, _, _ := rtlGetLastNtStatus.Call(); windows.NTStatus(st) == windows.STATUS_DELETE_PENDING {
			err = windows.ERROR_FILE_EXISTS
		}
	}
	return h, err
}

// retryDelays is how long Git for Windows waits before each retry of a file
// operation that failed because another process had a file open
// (retry_ask_yes_no in compat/mingw.c).
var retryDelays = [...]time.Duration{0, time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}

// retryInUse runs op, retrying after each of retryDelays while it fails with
// an error that another handle on the file can cause.
func retryInUse(op func() error) error {
	err := op()
	for _, d := range retryDelays {
		if err != windows.ERROR_ACCESS_DENIED && err != windows.ERROR_SHARING_VIOLATION {
			break
		}
		time.Sleep(d)
		err = op()
	}
	return err
}

// replaceFile renames oldpath over newpath as Git for Windows' mingw_rename
// does: with POSIX semantics, retrying while a process that does not share
// delete access holds either file open.
func replaceFile(oldpath, newpath string) error {
	err := retryInUse(func() error { return renamePOSIX(oldpath, newpath) })
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

// renamePOSIX renames oldpath over newpath with POSIX semantics, falling
// back to a classic rename where the filesystem does not support them.
func renamePOSIX(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16FromString(newpath)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(from, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	// FILE_RENAME_INFO, whose FileName runs past the end of the struct.
	type renameInfo struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [1]uint16
	}
	size := unsafe.Offsetof(renameInfo{}.FileName) + uintptr(len(to))*2
	buf := make([]uint64, (size+7)/8)
	info := (*renameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(len(to)-1) * 2
	copy(unsafe.Slice(&info.FileName[0], len(to)), to)
	err = windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, (*byte)(unsafe.Pointer(info)), uint32(size))
	windows.CloseHandle(h)
	switch err {
	case windows.ERROR_INVALID_PARAMETER, windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION:
		return windows.MoveFileEx(from, &to[0], windows.MOVEFILE_REPLACE_EXISTING)
	}
	return err
}
