//go:build linux

package checkout

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"strconv"
	"strings"
)

func stateLockInfo(f *os.File) (int, bool, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, false, err
	}
	data, err := os.ReadFile("/proc/locks")
	if err != nil {
		return 0, false, err
	}
	key := fmt.Sprintf("%02x:%02x:%d", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)), st.Ino)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[1] != "->" && fields[5] == key {
			pid, _ := strconv.Atoi(fields[4])
			return pid, true, nil
		}
	}
	return 0, false, nil
}
