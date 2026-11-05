package base

import "syscall"

// setStatMode stores mode bits in darwin's 16-bit field.
func setStatMode(s *syscall.Stat_t, mode uint32) { s.Mode = uint16(mode) }

// setStatMtime stores the modification time in darwin's field.
func setStatMtime(s *syscall.Stat_t, t syscall.Timespec) { s.Mtimespec = t }
