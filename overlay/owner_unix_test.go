//go:build !windows

package overlay

import (
	"fmt"
	"os"
	"syscall"
)

// owner renders a file's uid:gid, or "-" without host stat data.
func owner(info os.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
	}
	return "-"
}
