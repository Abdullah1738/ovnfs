//go:build !windows

package main

import (
	"net/url"
	"os"
	"runtime"
	"strconv"
)

// mountCmd builds the native client command for a localhost server.
// NFS mounts use v3 with client-local locks and normal attribute caching;
// refreshRoot makes checkout additions and removals visible immediately.
// extra holds generic mount(8) options, such as [atimeOption]'s.
// SMB uses NTLM credentials when configured, otherwise an unsigned guest.
// Linux cifs mounts belong to the daemon's user: the kernel gives a file it
// has just created the mount's owner and file_mode rather than the POSIX
// reply's, so a root-owned mount refuses `>` redirects into new files.
func mountCmd(proto string, port int, dir, extra, share, user, password string) []string {
	p := strconv.Itoa(port)
	if proto == "smb" {
		if runtime.GOOS == "darwin" {
			u := url.User("guest").String()
			if user != "" {
				u = url.UserPassword(user, password).String()
			}
			o := "nobrowse"
			if extra != "" {
				o += "," + extra
			}
			return []string{"mount_smbfs", "-N", "-o", o, "//" + u + "@127.0.0.1:" + p + "/" + url.PathEscape(share), dir}
		}
		o := "port=" + p + ",vers=3.1.1,"
		if user == "" {
			o += "guest"
		} else {
			o += "username=" + user + ",password=" + password
		}
		o += ",posix,cache=strict,actimeo=1,uid=" + strconv.Itoa(os.Geteuid()) + ",gid=" + strconv.Itoa(os.Getegid())
		if extra != "" {
			o += "," + extra
		}
		return []string{"mount", "-t", "cifs", "//127.0.0.1/" + share, dir, "-o", o}
	}
	var o string
	var cmd []string
	if runtime.GOOS == "darwin" {
		o = "vers=3,tcp,port=" + p + ",mountport=" + p + ",noresvport,locallocks,nobrowse,intr,rwsize=1048576"
		cmd = []string{"mount_nfs"}
	} else {
		o = "vers=3,tcp,port=" + p + ",mountport=" + p + ",nolock"
		cmd = []string{"mount", "-t", "nfs"}
	}
	if extra != "" {
		o += "," + extra
	}
	return append(cmd, "-o", o, "127.0.0.1:/", dir)
}

// atimeOption is the generic mount option for the access-time setting (serve
// -atime). Off by default: with access times, the macOS NFS client sends a
// SETATTR for every exec of a checkout binary.
func atimeOption(atime bool) string {
	if atime {
		return "atime"
	}
	return "noatime"
}
