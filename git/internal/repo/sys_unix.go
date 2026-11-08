//go:build unix

package repo

import (
	"context"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hostLookupTimeout bounds the canonical host name lookup made when no email
// address is configured.
const hostLookupTimeout = 2 * time.Second

// device returns the ID of the filesystem holding path.
func device(path string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

// ownedByCurrentUser reports whether path's owner is the effective user or,
// when running as root, the user named by SUDO_UID (config/safe.adoc).
func ownedByCurrentUser(path string) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	euid := os.Geteuid()
	if uint64(st.Uid) == uint64(euid) {
		return true, nil
	}
	if euid == 0 {
		if v, ok := os.LookupEnv("SUDO_UID"); ok {
			if uid, err := strconv.ParseUint(v, 10, 32); err == nil && uid == uint64(st.Uid) {
				return true, nil
			}
		}
	}
	return false, nil
}

// gecos returns the account's GECOS field.
func gecos(u *user.User) string { return u.Name }

// queryUserEmail returns the account's own email address, which only Git for
// Windows knows how to look up.
func queryUserEmail() string { return "" }

// canonicalName returns host's canonical name from the resolver.
func canonicalName(host string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), hostLookupTimeout)
	defer cancel()
	cname, err := net.DefaultResolver.LookupCNAME(ctx, host)
	if err != nil {
		return "", false
	}
	return strings.TrimSuffix(cname, "."), true
}
