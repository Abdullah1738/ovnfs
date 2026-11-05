package smb

import "github.com/go-git/go-billy/v5"

// Backend serves overlay-clean relative paths and stable file identifiers.
type Backend interface {
	billy.Filesystem
	billy.Change
	FileID(p string) uint64
	// Cacheable permits caching only when backend changes can call Server.Changed.
	Cacheable(p string) bool
	Statfs() (Statfs, error)
}

// Statfs describes filesystem capacity in units of BlockSize bytes.
type Statfs struct {
	BlockSize                             uint32
	Blocks, Free, Avail, Files, FilesFree uint64
}
