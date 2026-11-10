//go:build !darwin && !linux && !windows

package checkout

import (
	"fmt"
	"os"
)

func stateLockInfo(f *os.File) (int, bool, error) {
	return 0, false, fmt.Errorf("nonmutating state-lock queries are unsupported on this platform")
}
