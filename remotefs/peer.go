package remotefs

import (
	"fmt"
	"os"
	"slices"
)

// checkUID admits a peer whose uid is listed in allowed, or equals the server's effective uid when allowed is empty.
func checkUID(peer uint32, allowed []uint32) error {
	if len(allowed) == 0 {
		allowed = []uint32{uint32(os.Geteuid())}
	}
	if !slices.Contains(allowed, peer) {
		return fmt.Errorf("remotefs peer uid %d is not allowed (allowed: %v)", peer, allowed)
	}
	return nil
}
