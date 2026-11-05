package walk

import (
	"context"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
)

// Cache keeps directory listings between walks so that a directory which
// has not changed costs one metadata call (stat/statx/NtQueryFullAttributes)
// instead of open + list + close: on APFS ~1µs instead of ~12µs.
//
// Validity rests on the kernel's own change tracking. Adding, removing, or
// renaming an entry moves the directory's mtime and ctime; replacing the
// directory changes its inode. A cached listing is reused only while the
// directory's [fsys.Stamp] is unchanged, and ignore files are revalidated
// the same way. To rule out a change landing in the same timestamp tick as
// the listing, a listing is cached only once its stamp is older than
// racyWindow (git's "racily clean" rule); younger directories are simply
// listed again.
//
// Only filesystems whose sessions report [fsys.Session.StableStamps] take
// part (natively: APFS; ext4, XFS, Btrfs, tmpfs, F2FS, ZFS, bcachefs; NTFS,
// ReFS). Network, FUSE, and overlay mounts are always listed. Only
// Minimal-detail walks use the cache: file sizes and mtimes change without
// touching their directory's stamp.
//
// A Cache is safe for concurrent walks. Pass the same one to every walk
// that should share it via Options.Cache.
type Cache struct {
	seed     maphash.Seed
	shards   [cacheShards]cacheShard
	bytes    atomic.Int64
	maxBytes int64
}

// NewCache returns a cache that holds at most about maxBytes of listings,
// dropping whole shards when full.
func NewCache(maxBytes int64) *Cache {
	c := &Cache{seed: maphash.MakeSeed(), maxBytes: maxBytes}
	for i := range c.shards {
		c.shards[i].m = make(map[string]*cachedDir)
	}
	return c
}

const cacheShards = 64

// racyWindow is how old a stamp must be before its listing is trusted.
// It covers the coarsest timestamp clock among the allowed filesystems
// (Linux's tick-granular coarse clock, Windows' 15.6ms timer).
var racyWindow = 50 * time.Millisecond

type cacheShard struct {
	mu sync.RWMutex
	m  map[string]*cachedDir // absolute path → listing
}

// cachedDir is an immutable listing snapshot; updates replace it.
type cachedDir struct {
	st      fsys.Stamp
	listing *fsys.Listing
	flags   entryNames
	// ignores is set when the filling walk honoured ignore files.
	ignores *dirIgnores
	size    int64
}

func (c *Cache) shard(key []byte) *cacheShard {
	return &c.shards[maphash.Bytes(c.seed, key)%cacheShards]
}

func (c *Cache) get(key []byte) *cachedDir {
	s := c.shard(key)
	s.mu.RLock()
	d := s.m[string(key)]
	s.mu.RUnlock()
	return d
}

func (c *Cache) put(key []byte, d *cachedDir) {
	d.size = int64(len(key)+d.listing.Size()) + 128
	s := c.shard(key)
	s.mu.Lock()
	old := s.m[string(key)]
	s.m[string(key)] = d
	s.mu.Unlock()
	delta := d.size
	if old != nil {
		delta -= old.size
	}
	if c.bytes.Add(delta) > c.maxBytes {
		c.evict(s)
	}
}

func (c *Cache) drop(key []byte) {
	s := c.shard(key)
	s.mu.Lock()
	if old := s.m[string(key)]; old != nil {
		delete(s.m, string(key))
		c.bytes.Add(-old.size)
	}
	s.mu.Unlock()
}

// evict empties shards, starting after the one just written, until the
// cache fits its budget again.
func (c *Cache) evict(written *cacheShard) {
	start := 0
	for i := range c.shards {
		if &c.shards[i] == written {
			start = i + 1
		}
	}
	for i := range cacheShards {
		if c.bytes.Load() <= c.maxBytes {
			return
		}
		s := &c.shards[(start+i)%cacheShards]
		s.mu.Lock()
		var freed int64
		for _, d := range s.m {
			freed += d.size
		}
		clear(s.m)
		s.mu.Unlock()
		c.bytes.Add(-freed)
	}
}

// settled reports whether stamps taken no later than now are old enough
// that a later change must produce a different stamp.
func settled(now int64, stamps ...fsys.Stamp) bool {
	for _, s := range stamps {
		if s.Latest() >= now-int64(racyWindow) {
			return false
		}
	}
	return true
}

// cachedListing returns the cached listing of the directory at abs (whose
// separator is sep) if it is still current, revalidating its ignore sources
// when withIgnores is set.
func (c *Cache) cachedListing(ctx context.Context, s fsys.Session, abs []byte, sep byte, withIgnores bool) *cachedDir {
	if ctx.Err() != nil {
		return nil
	}
	d := c.get(abs)
	if d == nil || (withIgnores && d.ignores == nil) {
		return nil
	}
	// Follow: a directory reached through a followed link is stored under
	// the link's path with the target's stamp.
	info, err := s.Stat(unsafe.String(unsafe.SliceData(abs), len(abs)), true)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil || info.Stamp != d.st {
		c.drop(abs)
		return nil
	}
	if withIgnores {
		var path []byte
		for k, name := range ignoreSources {
			if ctx.Err() != nil {
				return nil
			}
			if d.flags&sourceFlags[k] == 0 {
				continue
			}
			path = append(append(append(path[:0], abs...), sep), name...)
			info, err := s.Stat(unsafe.String(unsafe.SliceData(path), len(path)), true)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				info.Stamp = fsys.Stamp{}
			}
			if info.Stamp != d.ignores.stamps[k] {
				return nil
			}
		}
	}
	return d
}

// store caches the listing just read from h if its stamps have settled.
func (c *Cache) store(ctx context.Context, s fsys.Session, abs []byte, h fsys.Handle, st fsys.Stamp, ls *fsys.Listing, flags entryNames, ign *dirIgnores) {
	if ctx.Err() != nil {
		return
	}
	now := time.Now().UnixNano()
	if !settled(now, st) || (ign != nil && !settled(now, ign.stamps[:]...)) || !s.StableStamps(h, st) {
		return
	}
	d := &cachedDir{st: st, listing: ls.Clone(), flags: flags, ignores: ign}
	for i := range d.listing.Entries {
		if ctx.Err() != nil {
			return
		}
		// Listings that carry sizes and mtimes anyway (Windows) must not
		// replay them later: those change without touching the directory.
		r := &d.listing.Entries[i]
		r.Size, r.Mtime, r.Children = -1, 0, 0
	}
	c.put(abs, d)
}
