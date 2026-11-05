//go:build windows

package export

import (
	"github.com/stencil-hq/ovnfs/smb"
	"golang.org/x/sys/windows"
)

// syncNodeDirectory skips directory fsync, which Windows does not support.
func syncNodeDirectory(string) error { return nil }

// volumeStatfs expresses Windows byte capacities in 4 KiB accounting blocks.
// Windows does not expose a filesystem-wide free inode count.
func volumeStatfs(dir string) (smb.Statfs, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return smb.Statfs{}, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return smb.Statfs{}, err
	}
	return smb.Statfs{BlockSize: 4096, Blocks: total / 4096, Free: free / 4096, Avail: avail / 4096}, nil
}
