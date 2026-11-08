package overlay

import (
	"os"
	"path"
)

// inodeKey identifies one host inode so hardlinked paths share metadata updates.
type inodeKey struct{ device, inode uint64 }

// inodeNames stores ordinary single-linked files without a per-file map allocation.
type inodeNames struct {
	first  string
	others map[string]struct{}
}

// addIdentity records a name, allocating an alias set only for real hardlinks.
func (f *FS) addIdentity(p string, key inodeKey) {
	names := f.aliases[key]
	if names.first == "" {
		names.first = p
	} else if names.first != p {
		if names.others == nil {
			names.others = make(map[string]struct{})
		}
		names.others[p] = struct{}{}
	}
	f.aliases[key] = names
	f.pathIDs[p] = key
}

// track records one upper name's inode without reading or changing its metadata.
func (f *FS) track(p string, info os.FileInfo) error {
	if info == nil {
		return nil
	}
	if err := primeInfo(info); err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	key, err := identity(f.upper, p, info)
	if err != nil {
		return err
	}
	if old, ok := f.pathIDs[p]; ok {
		if old == key {
			return nil
		}
		f.untrack(p)
	}
	f.addIdentity(p, key)
	return nil
}

// untrack retires one upper name without retaining dead inode groups.
func (f *FS) untrack(p string) {
	key, ok := f.pathIDs[p]
	if !ok {
		return
	}
	delete(f.pathIDs, p)
	names := f.aliases[key]
	if names.first == p {
		names.first = ""
		for other := range names.others {
			names.first = other
			delete(names.others, other)
			break
		}
	} else {
		delete(names.others, p)
	}
	if names.first == "" {
		delete(f.aliases, key)
	} else {
		if len(names.others) == 0 {
			names.others = nil
		}
		f.aliases[key] = names
	}
}

// dropIndex removes a subtree and all its hardlink bookkeeping together.
func (f *FS) dropIndex(p string) {
	for n := range f.index.children[p] {
		f.dropIndex(path.Join(p, n))
	}
	if m := f.files[p]; m != nil {
		f.detach(m)
	}
	f.untrack(p)
	f.index.drop(p)
}

// otherAlias finds a surviving name whose link count changes when p is removed.
func (f *FS) otherAlias(p string) string {
	key, ok := f.pathIDs[p]
	if !ok {
		return ""
	}
	names := f.aliases[key]
	if names.first != p {
		return names.first
	}
	for other := range names.others {
		return other
	}
	return ""
}

// moveIdentity temporarily registers a renamed path before the old name retires.
func (f *FS) moveIdentity(from, to string) {
	if key, ok := f.pathIDs[from]; ok {
		f.addIdentity(to, key)
	}
}

// restat re-reads p's metadata after a descriptor changed the inode's content
// or attributes and shares it with p's other hard-link names; the inode, and
// so the identity bookkeeping, is unchanged.
func (f *FS) restat(p string) error {
	info, err := hostLstat(f.up(p))
	if err != nil {
		return err
	}
	if err := primeInfo(info); err != nil {
		return err
	}
	f.setInfo(p, info)
	f.cacheAliases(p, info)
	return nil
}

// setInfo indexes upper name p's metadata, keeping its opacity.
func (f *FS) setInfo(p string, info os.FileInfo) {
	e := entry(info)
	e.Opaque = f.index.entries[p].Opaque
	f.index.set(p, e)
}

// cacheInfo stores a fresh snapshot and shares it with other names of the same inode.
func (f *FS) cacheInfo(p string, info os.FileInfo) error {
	f.setInfo(p, info)
	if err := f.track(p, info); err != nil {
		return err
	}
	f.cacheAliases(p, info)
	return nil
}

// cacheAliases gives p's other hard-link names info, p's fresh metadata. The
// names share one inode, so only the name differs; re-reading each would cost
// a host lstat per link on every metadata change.
func (f *FS) cacheAliases(p string, info os.FileInfo) {
	key, ok := f.pathIDs[p]
	if !ok {
		return
	}
	names := f.aliases[key]
	if names.first != p {
		f.setInfo(names.first, aliasInfo{FileInfo: info, name: path.Base(names.first)})
	}
	for other := range names.others {
		if other != p {
			f.setInfo(other, aliasInfo{FileInfo: info, name: path.Base(other)})
		}
	}
}

// aliasInfo is a hard link's metadata read through another of its names.
type aliasInfo struct {
	os.FileInfo
	name string
}

// Name returns the alias's own base name.
func (i aliasInfo) Name() string { return i.name }

// unalias returns the host metadata beneath an [aliasInfo], whose own type
// [os.SameFile] requires.
func unalias(info os.FileInfo) os.FileInfo {
	if a, ok := info.(aliasInfo); ok {
		return a.FileInfo
	}
	return info
}

// bind associates an open descriptor with its inode, not its current name; a
// writable one makes the inode's indexed metadata volatile until it closes.
func (f *FS) bind(h *file, p string) {
	h.inode, h.tracked = f.pathIDs[p]
	if h.writable && h.tracked {
		f.writers[h.inode]++
	}
}
