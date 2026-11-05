// Package ntinfo describes export files in Windows NT terms (FILETIME stamps,
// FILE_ATTRIBUTE_* masks, allocation sizes, symlink reparse buffers, NTSTATUS
// codes) for the frontends that serve Windows clients: the SMB server and the
// WinFsp drive.
package ntinfo

import (
	"os"
	"reflect"
	"time"
)

// windowsEpoch is the number of seconds from 1601 to the Unix epoch.
const windowsEpoch int64 = 11644473600

// Filetime encodes a time in 100-nanosecond intervals since 1601 for SMB and WinFsp stamps.
func Filetime(t time.Time) uint64 {
	seconds := t.Unix() + windowsEpoch
	if seconds < 0 {
		return 0
	}
	return uint64(seconds)*10000000 + uint64(t.Nanosecond()/100)
}

// FromFiletime converts a client FILETIME to UTC without overflowing UnixNano.
func FromFiletime(v uint64) time.Time {
	return time.Unix(int64(v/10000000)-windowsEpoch, int64(v%10000000)*100).UTC()
}

// Attributes maps filesystem mode bits to the FILE_ATTRIBUTE_* mask Windows clients see.
func Attributes(info os.FileInfo) uint32 {
	var a uint32 = 0x20
	if info.IsDir() {
		a = 0x10
	}
	if info.Mode()&os.ModeSymlink != 0 {
		a |= 0x400
	}
	if info.Mode().Perm()&0200 == 0 {
		a |= 1
	}
	return a
}

// AllocationSize reports Windows allocation, not host sparse/compressed storage usage.
// Regular files are not advertised as sparse or compressed, so their allocation
// must cover EOF even when the host has not physically allocated every extent.
func AllocationSize(info os.FileInfo) uint64 {
	allocated, native := platformAllocation(info.Sys())
	if native && !info.Mode().IsRegular() {
		return allocated
	}
	if info.Size() > 0 {
		logical := (uint64(info.Size()) + 4095) &^ 4095
		if logical > allocated {
			return logical
		}
	}
	return allocated
}

// ChangeTime reports native inode change time, falling back to modification time.
func ChangeTime(info os.FileInfo) time.Time {
	if t, ok := platformChangeTime(info.Sys()); ok {
		return t
	}
	return info.ModTime()
}

// BirthTime reads the native creation timestamp, falling back to modification time.
func BirthTime(info os.FileInfo) time.Time { return metadataTime(info, true) }

// AccessTime reads the native access timestamp, falling back to modification time.
func AccessTime(info os.FileInfo) time.Time { return metadataTime(info, false) }

// metadataTime reads a native access or birth timestamp, falling back to modification time.
func metadataTime(info os.FileInfo, birth bool) time.Time {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return info.ModTime()
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return info.ModTime()
	}
	names := []string{"Atim", "Atimespec"}
	if birth {
		names = []string{"Birthtimespec", "Birthtim"}
	}
	for _, name := range names {
		f := v.FieldByName(name)
		if f.IsValid() && f.Kind() == reflect.Struct {
			sec, nsec := f.FieldByName("Sec"), f.FieldByName("Nsec")
			if sec.IsValid() && nsec.IsValid() && sec.CanInt() && nsec.CanInt() {
				return time.Unix(sec.Int(), nsec.Int())
			}
		}
	}
	return info.ModTime()
}
