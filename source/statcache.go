package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/store"
)

// statEntry remembers a file's stat data and the blob id its content hashed to.
type statEntry struct {
	Size  int64     `json:"s"`
	Mtime int64     `json:"m"`
	Ctime int64     `json:"c"`
	Ino   uint64    `json:"i"`
	Mode  uint32    `json:"x"`
	OID   store.OID `json:"o"`
}

// fileKey is the identity that detects a replaced or rewritten file: its inode
// (NTFS file id on Windows) and inode-change time; zero where the host has none.
type fileKey struct {
	ino   uint64
	ctime int64
}

// newStatEntry records the stat data that must be unchanged for id to be reused.
func newStatEntry(info os.FileInfo, key fileKey, mode uint32, id store.OID) statEntry {
	return statEntry{Size: info.Size(), Mtime: info.ModTime().UnixNano(), Ctime: key.ctime, Ino: key.ino, Mode: mode, OID: id}
}

// matches reports a file whose stat data equals the recorded one.
func (e statEntry) matches(info os.FileInfo, key fileKey, mode uint32) bool {
	return e.Size == info.Size() && e.Mtime == info.ModTime().UnixNano() && e.Ctime == key.ctime && e.Ino == key.ino && e.Mode == mode
}

// statCache is a source folder's stat cache: entries whose blobs were all in the
// store during the store epoch Epoch.
type statCache struct {
	Epoch   string               `json:"epoch"`
	Entries map[string]statEntry `json:"entries"`
}

// loadStatCache reads a source folder's stat cache; a missing or damaged file is an empty cache.
func loadStatCache(name string) statCache {
	empty := statCache{Entries: map[string]statEntry{}}
	if name == "" {
		return empty
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return empty
	}
	var c statCache
	if json.Unmarshal(data, &c) != nil || c.Entries == nil {
		return empty
	}
	return c
}

// saveStatCache atomically replaces the cache, leaving out entries modified within
// two seconds of cutoff: on coarse-timestamp filesystems their mtime cannot prove
// that a later write did not happen (git's racy-clean rule).
func saveStatCache(name string, entries map[string]statEntry, cutoff time.Time, epoch string) error {
	limit := cutoff.Add(-2 * time.Second).UnixNano()
	kept := statCache{Epoch: epoch, Entries: make(map[string]statEntry, len(entries))}
	for p, e := range entries {
		if e.Mtime < limit {
			kept.Entries[p] = e
		}
	}
	data, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// indexClean reports a regular file whose stat data still equals its git index
// entry, so its content is the entry's blob before any conversion git applies
// between blob and work tree; racily clean entries never count.
func indexClean(e git.IndexEntry, info os.FileInfo, key fileKey, mode uint32, indexModified time.Time) bool {
	if e.Mode != mode || int64(e.Size) != info.Size()&0xffffffff || !info.ModTime().Before(indexModified) {
		return false
	}
	if e.Inode != 0 && e.Inode != uint32(key.ino) {
		return false
	}
	return sameTime(e.Mtime, info.ModTime()) && (key.ctime == 0 || sameTime(e.Ctime, time.Unix(0, key.ctime)))
}

// sameTime compares index times, which may lack nanoseconds.
func sameTime(index, actual time.Time) bool {
	if index.Unix() != actual.Unix() {
		return false
	}
	return index.Nanosecond() == 0 || index.Nanosecond() == actual.Nanosecond()
}
