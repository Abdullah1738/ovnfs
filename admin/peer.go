//go:build darwin || linux

package admin

import "fmt"

// checkUID rejects a socket owned by another effective user.
func checkUID(peer, own uint32) error {
	if peer != own {
		return fmt.Errorf("admin peer uid %d does not match server uid %d", peer, own)
	}
	return nil
}
