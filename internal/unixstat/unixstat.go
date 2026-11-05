// Package unixstat reads the POSIX metadata that os.FileInfo keeps in Sys,
// for the frontends that report it (NFS, FUSE, remote-fs).
package unixstat

import "os"

// Info is the POSIX metadata a frontend reports beyond os.FileInfo.
type Info struct {
	Nlink    uint32
	UID, GID uint32
	// Major and Minor are a block or character special file's device number.
	Major, Minor uint32
}

// Of extracts info's POSIX metadata from a native stat record in Sys, or an
// Info there (as test fakes supply); ok is false when Sys holds neither.
func Of(info os.FileInfo) (Info, bool) {
	switch v := info.Sys().(type) {
	case Info:
		return v, true
	case *Info:
		if v != nil {
			return *v, true
		}
		return Info{}, false
	}
	return native(info.Sys())
}
