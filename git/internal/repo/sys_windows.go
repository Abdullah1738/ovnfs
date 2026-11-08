//go:build windows

package repo

import (
	"os/user"

	"golang.org/x/sys/windows"
)

// device is not available on Windows, so discovery does not stop at
// filesystem boundaries there.
func device(string) (uint64, bool) { return 0, false }

// ownedByCurrentUser reports whether path's owner SID is the current user's,
// or is BUILTIN\Administrators while the current user is a member of that
// group. A path whose filesystem records no owner counts as owned.
func ownedByCurrentUser(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	if owner == nil {
		return true, nil
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	if owner.Equals(user.User.Sid) {
		return true, nil
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	if !owner.Equals(admins) {
		return false, nil
	}
	// A zero token checks the calling thread's (or process's) own token.
	return windows.Token(0).IsMember(admins)
}

// gecos returns what Git for Windows' getpwuid reports as the GECOS field:
// the account's display name, or "unknown" when it has none.
func gecos(*user.User) string {
	if name, ok := userNameEx(windows.NameDisplay); ok {
		return name
	}
	return "unknown"
}

// queryUserEmail returns the account's user principal name, which Git for
// Windows prefers to making up an address, or "".
func queryUserEmail() string {
	email, _ := userNameEx(windows.NameUserPrincipal)
	return email
}

// userNameEx returns the current account's name in format, reading it into
// a buffer of git's size.
func userNameEx(format uint32) (string, bool) {
	var buf [1024]uint16
	n := uint32(len(buf))
	if err := windows.GetUserNameEx(format, &buf[0], &n); err != nil {
		return "", false
	}
	return windows.UTF16ToString(buf[:n]), true
}

// canonicalName returns host's canonical name as getaddrinfo with
// AI_CANONNAME reports it, the lookup Git for Windows makes.
func canonicalName(host string) (string, bool) {
	node, err := windows.UTF16PtrFromString(host)
	if err != nil {
		return "", false
	}
	hints := windows.AddrinfoW{Flags: windows.AI_CANONNAME}
	var res *windows.AddrinfoW
	if err := windows.GetAddrInfoW(node, nil, &hints, &res); err != nil {
		return "", false
	}
	defer windows.FreeAddrInfoW(res)
	if res.Canonname == nil {
		return "", false
	}
	return windows.UTF16PtrToString(res.Canonname), true
}
