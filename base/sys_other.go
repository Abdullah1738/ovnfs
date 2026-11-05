//go:build !darwin && !linux

package base

import (
	"os"
	"time"
)

// syntheticSys has no host stat form here; frontends fall back to their defaults.
func syntheticSys(os.FileMode, int64, time.Time) any { return nil }
