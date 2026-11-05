package base

import "syscall"

// setStatMode stores mode bits in linux's 32-bit field.
func setStatMode(s *syscall.Stat_t, mode uint32) { s.Mode = mode }

// setStatMtime stores the modification time in linux's field.
func setStatMtime(s *syscall.Stat_t, t syscall.Timespec) { s.Mtim = t }
