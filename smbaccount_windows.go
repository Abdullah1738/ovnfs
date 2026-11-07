//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// smbUser is the one account the daemon's SMB server accepts on Windows.
const smbUser = "ovnfs"

// smbPasswordFile names the state directory file holding smbUser's password.
const smbPasswordFile = "smb-password"

// smbAccount returns the account the SMB server accepts and the drive mapping
// presents: Windows 11 24H2 and Server 2025 refuse anonymous NTLM, guest
// logons and unsigned sessions by default. The password is created once per
// state directory, so a mapping left by a previous run authenticates to the next.
func smbAccount(state string) (user, password string, err error) {
	path := filepath.Join(state, smbPasswordFile)
	b, err := os.ReadFile(path)
	if err == nil {
		if _, decodeErr := hex.DecodeString(string(b)); decodeErr != nil || len(b) != 32 {
			return "", "", fmt.Errorf("%s does not hold a 32-digit hex password; delete it to create a new one", path)
		}
		return smbUser, string(b), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("reading SMB password: %w", err)
	}
	key := make([]byte, 16)
	rand.Read(key) // never fails since Go 1.24
	password = hex.EncodeToString(key)
	// A run that dies mid-write leaves only the temporary file, which the next run replaces.
	tmp := path + ".tmp"
	if err = os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("SMB password: %w", err)
	}
	if err = writeOwnerOnly(tmp, []byte(password)); err != nil {
		return "", "", fmt.Errorf("SMB password: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return "", "", fmt.Errorf("SMB password: %w", err)
	}
	return smbUser, password, nil
}

// writeOwnerOnly creates path holding data under a protected DACL that grants
// the current user alone access: mode bits do not restrict Windows files, and
// the state directory's inherited ACL may admit other users.
func writeOwnerOnly(path string, data []byte) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return fmt.Errorf("security descriptor: %w", err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return &fs.PathError{Op: "create", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
