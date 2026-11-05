//go:build !darwin && !linux

package ntinfo

import "time"

// platformChangeTime lets unsupported platforms fall back to file mtime.
func platformChangeTime(value any) (time.Time, bool) { return time.Time{}, false }

// platformAllocation lets unsupported platforms use block-rounded file sizes.
func platformAllocation(value any) (uint64, bool) { return 0, false }
