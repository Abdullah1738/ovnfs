package clone

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneInto shares in's extents with the empty out through FICLONE, which
// works on the open inode, so a path swapped after open cannot redirect the clone.
func cloneInto(out, in *os.File) error {
	return unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
}
