package store

import (
	"container/list"
	"os"
	"sync"
)

// cacheObjectLimit is the largest object kept in memory; bigger blobs are
// served by Open each time instead of pinning RAM.
const cacheObjectLimit = 1 << 20

// blobCache keeps small immutable object bodies in memory by id, evicting the
// least recently used once their total size exceeds budget. A nil cache is off.
type blobCache struct {
	budget int64
	mu     sync.Mutex
	used   int64
	byID   map[OID]*list.Element
	lru    list.List // of *cacheEntry, most recently used first
}

// cacheEntry is one resident object body.
type cacheEntry struct {
	id   OID
	data []byte
}

// newBlobCache returns a cache holding at most budget bytes, or nil when budget disables it.
func newBlobCache(budget int64) *blobCache {
	if budget <= 0 {
		return nil
	}
	return &blobCache{budget: budget, byID: make(map[OID]*list.Element)}
}

// fits reports whether an object of size bytes may be admitted.
func (c *blobCache) fits(size int64) bool {
	return c != nil && size <= cacheObjectLimit && size <= c.budget
}

// get returns id's resident body and marks it recently used.
func (c *blobCache) get(id OID) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byID[id]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*cacheEntry).data, true
}

// add makes data, which the cache now owns and nobody may modify, resident as
// id's body unless it does not fit; it returns the resident body, which is an
// earlier copy when another loader won the race.
func (c *blobCache) add(id OID, data []byte) []byte {
	if !c.fits(int64(len(data))) {
		return data
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byID[id]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*cacheEntry).data
	}
	c.byID[id] = c.lru.PushFront(&cacheEntry{id: id, data: data})
	c.used += int64(len(data))
	for c.used > c.budget {
		c.drop(c.lru.Back())
	}
	return data
}

// remove forgets id's body, if resident.
func (c *blobCache) remove(id OID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byID[id]; ok {
		c.drop(el)
	}
}

// drop unlinks one entry; c.mu must be held.
func (c *blobCache) drop(el *list.Element) {
	e := c.lru.Remove(el).(*cacheEntry)
	delete(c.byID, e.id)
	c.used -= int64(len(e.data))
}

// resident returns the bytes currently held.
func (c *blobCache) resident() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// CacheUsage reports the bytes of blobs held in memory and the cache's budget (0 when off).
func (s *Store) CacheUsage() (used, limit int64) {
	if s.cache == nil {
		return 0, 0
	}
	return s.cache.resident(), s.cache.budget
}

// Cached returns a small stored blob's bytes from memory, loading it from its pack
// or object file on first use. ok is false when the cache is off, the object is larger than the
// per-object limit, or it is missing. The slice is shared and immutable: callers must not modify it.
func (s *Store) Cached(id OID) (data []byte, ok bool) {
	if s.cache == nil || !id.Valid() {
		return nil, false
	}
	if data, ok := s.cache.get(id); ok {
		return data, true
	}
	// Loading under the publication lock orders it against Sweep, which
	// drops what it deletes: a deleted object is never made resident again.
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.packs.lookup(rawOf(id)); ok {
		if !s.cache.fits(int64(e.raw)) {
			return nil, false
		}
		data, ok, err := s.readPacked(rawOf(id))
		if !ok || err != nil {
			return nil, false
		}
		return s.cache.add(id, data), true
	}
	p := s.objectPath(id)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || !s.cache.fits(info.Size()) {
		return nil, false
	}
	data, err = os.ReadFile(p)
	if err != nil || int64(len(data)) != info.Size() {
		return nil, false
	}
	return s.cache.add(id, data), true
}
