//go:build windows

package admin

import (
	"fmt"
	"net"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sioAFUnixGetPeerPID is SIO_AF_UNIX_GETPEERPID (_WSAIOR(IOC_VENDOR, 256)): the
// id of the process that connected, recorded by afunix.sys at connect time.
const sioAFUnixGetPeerPID = 0x58000100

// ownUser is this process's token user, the only user a peer may run as.
var ownUser = sync.OnceValues(func() (*windows.Tokenuser, error) {
	return windows.GetCurrentProcessToken().GetTokenUser()
})

// CheckPeer rejects a Unix socket peer whose process token user differs from
// this process's. It fails when the peer pid is unavailable, or when the peer
// process has exited or cannot be queried (a protected process). A peer that
// exits and whose pid is reused before the check is judged by the new process;
// the socket file's ACL is the first line of defense.
func CheckPeer(conn net.Conn) error {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("admin requires a Unix connection")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var pid uint32
	var ioctlErr error
	err = raw.Control(func(fd uintptr) {
		var n uint32
		ioctlErr = windows.WSAIoctl(windows.Handle(fd), sioAFUnixGetPeerPID, nil, 0, (*byte)(unsafe.Pointer(&pid)), uint32(unsafe.Sizeof(pid)), &n, nil, 0)
	})
	if err != nil {
		return err
	}
	if ioctlErr != nil {
		return fmt.Errorf("socket peer pid: %w", ioctlErr)
	}
	own, err := ownUser()
	if err != nil {
		return fmt.Errorf("own token user: %w", err)
	}
	peer, err := processUser(pid)
	if err != nil {
		return fmt.Errorf("socket peer pid %d: %w", pid, err)
	}
	if !windows.EqualSid(peer.User.Sid, own.User.Sid) {
		return fmt.Errorf("socket peer user %s does not match server user %s", peer.User.Sid, own.User.Sid)
	}
	return nil
}

// processUser reads the user of a process's primary token.
func processUser(pid uint32) (*windows.Tokenuser, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	return token.GetTokenUser()
}
