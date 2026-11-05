package export

import (
	"os"
	"strings"

	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/smb"
	"github.com/go-git/go-billy/v5"
)

// protocolFS is the export as a network protocol server sees it: node
// bookkeeping without the recall hook (each server recalls its own clients'
// caches), stable file ids, the caching policy and volume capacity.
type protocolFS struct {
	*nodeFS
	volume  string
	tracked func(checkout, p string) bool // nil: nothing is cacheable
}

// protocolFS shares e's filesystem and node table, without its recall hook.
func (e *Export) protocolFS() protocolFS {
	s := protocolFS{
		nodeFS: &nodeFS{Filesystem: e.fs.Filesystem, Change: e.fs.Change, nodes: e.fs.nodes},
		volume: e.volume,
	}
	if e.rt != nil {
		s.tracked = e.rt.reg.Tracked
	}
	return s
}

// FileID returns the stable node id that NFS handles and SMB file ids carry.
func (s *protocolFS) FileID(p string) uint64 { return s.nodes.id(overlay.Clean(p)) }

// Cacheable permits leases and delegations only when backend changes can
// recall cached contents (see checkout.Registry.Tracked).
func (s *protocolFS) Cacheable(p string) bool {
	checkout, rel, ok := strings.Cut(overlay.Clean(p), "/")
	return ok && s.tracked != nil && s.tracked(checkout, rel)
}

// Statfs reports space on upper's volume, where all overlay writes land.
func (s *protocolFS) Statfs() (smb.Statfs, error) { return volumeStatfs(s.volume) }

// smbFS is the export as the SMB server sees it.
type smbFS struct{ protocolFS }

// SMB exposes stable file identities; SMB itself recalls conflicting client caches.
func (e *Export) SMB() smb.Backend { return &smbFS{e.protocolFS()} }

// OpenFile opens without nodeFS's recall: the SMB server breaks conflicting
// caches itself before a CREATE opens for writing, and the recall would come
// back into the same server as an extra lease scan per such open.
func (s *smbFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	return s.nodeFS.Filesystem.OpenFile(name, flag, perm)
}

// The adapter exposes filesystem operations and metadata mutations.
var _ smb.Backend = (*smbFS)(nil)
