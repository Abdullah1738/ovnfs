//go:build linux

package ntinfo

import (
	"syscall"
	"time"
)

// platformChangeTime extracts Linux stat ctime without reflection.
func platformChangeTime(value any) (time.Time, bool) {
	switch s := value.(type) {
	case *syscall.Stat_t:
		if s != nil {
			return time.Unix(int64(s.Ctim.Sec), int64(s.Ctim.Nsec)), true
		}
	case syscall.Stat_t:
		return time.Unix(int64(s.Ctim.Sec), int64(s.Ctim.Nsec)), true
	}
	return time.Time{}, false
}

// platformAllocation extracts POSIX allocation measured in 512-byte blocks.
func platformAllocation(value any) (uint64, bool) {
	switch s := value.(type) {
	case *syscall.Stat_t:
		if s != nil && s.Blocks >= 0 {
			return uint64(s.Blocks) * 512, true
		}
	case syscall.Stat_t:
		if s.Blocks >= 0 {
			return uint64(s.Blocks) * 512, true
		}
	}
	return 0, false
}
