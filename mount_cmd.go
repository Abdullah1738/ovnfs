package main

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// mountCmd builds the native client command for a localhost server.
// NFS mounts use v3 with client-local locks and normal attribute caching;
// refreshRoot makes checkout additions and removals visible immediately.
// SMB uses NTLM credentials when configured, otherwise an unsigned guest.
// Linux cifs mounts belong to the daemon's user: the kernel gives a file it
// has just created the mount's owner and file_mode rather than the POSIX
// reply's, so a root-owned mount refuses `>` redirects into new files.
func mountCmd(proto string, port int, dir, extra, share, user, password string) []string {
	p := strconv.Itoa(port)
	if proto == "smb" {
		if runtime.GOOS == "windows" {
			cmd := []string{"net", "use", filepath.VolumeName(dir), `\\127.0.0.1\` + share, password, "/USER:" + user, "/persistent:no"}
			// Only Windows 11 24H2 and later dial another port; 445 works on any client.
			if port != 445 {
				cmd = append(cmd, "/TCPPORT:"+p)
			}
			return cmd
		}
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
