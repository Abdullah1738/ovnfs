//go:build !windows

package export

import (
	"github.com/stencil-hq/ovnfs/internal/durable"
	"github.com/stencil-hq/ovnfs/smb"
	"golang.org/x/sys/unix"
)

// syncNodeDirectory makes the compacted log's directory entry durable.
func syncNodeDirectory(dir string) error {
	return durable.Dir(dir)
}

// volumeStatfs reads filesystem capacity on the volume containing dir.
func volumeStatfs(dir string) (smb.Statfs, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return smb.Statfs{}, err
	}
	return smb.Statfs{
		BlockSize: uint32(st.Bsize), Blocks: st.Blocks,
		Free: st.Bfree, Avail: st.Bavail, Files: st.Files, FilesFree: st.Ffree,
	}, nil
}
