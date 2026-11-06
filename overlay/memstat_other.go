//go:build !darwin && !linux

package overlay

import (
	"os"
	"time"
)

// umask emulates the common Unix creation mask where the platform has none, so
// files created through the overlay get the modes a Unix host would give them
// (0644 for 0666), which on Windows are also the defaults needing no stored mode.
const umask os.FileMode = 0o022

// memStat has no host stat layout to imitate on this platform.
func memStat(memInfo) any { return nil }

// newOwner has no numeric owners to report on this platform.
func newOwner(os.FileInfo) (uid, gid uint32) { return 0, 0 }

// matchOwner leaves ownership to the platform.
func matchOwner(*os.File, uint32, uint32) error { return nil }

// birthTime reports no creation time to restore: the platform keeps its own.
func birthTime(os.FileInfo) (time.Time, bool) { return time.Time{}, false }

// setCreationTime leaves creation times to the platform.
func setCreationTime(string, time.Time) error { return nil }
