//go:build windows

package repo

import (
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
