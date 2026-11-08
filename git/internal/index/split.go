package index

import (
	"fmt"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// splitLink is the link extension of a split index: the shared index it
// builds on and the still-encoded bitmaps of shared entries it deletes and
// replaces, decoded once the shared index's size is known.
type splitLink struct {
	shared  object.ID
	bitmaps []byte
}

func parseLink(b []byte, f object.Format) (*splitLink, error) {
	if len(b) < f.Size() {
		return nil, fmt.Errorf("size %d", len(b))
	}
	l := &splitLink{bitmaps: b[f.Size():]}
	l.shared, _ = object.IDFromRaw(f, b[:f.Size()])
	return l, nil
}

// applySplit merges the split index idx, whose entries are still the stored
// replacements followed by additions, with its shared index.
func (idx *Index) applySplit(l *splitLink, shared *Index) error {
	base := shared.Entries
	var deleted, replace bitmap
	if b := l.bitmaps; len(b) > 0 {
		var err error
		if deleted, b, err = decodeEWAH(b, len(base)); err != nil {
			return fmt.Errorf("link delete bitmap: %w", err)
		}
		if replace, b, err = decodeEWAH(b, len(base)); err != nil {
			return fmt.Errorf("link replace bitmap: %w", err)
		}
		if len(b) != 0 {
			return fmt.Errorf("link extension: %d trailing bytes", len(b))
		}
	}
	stored := idx.Entries
	next := 0
	for pos := range replace.ones() {
		if pos >= len(base) {
			return fmt.Errorf("split index replaces shared entry %d of %d", pos, len(base))
		}
		if next >= len(stored) {
			return fmt.Errorf("split index lacks replacement entries")
		}
		e := stored[next]
		next++
		// Replacements may omit their name to save space.
		if e.Path == "" {
			e.Path = base[pos].Path
		}
		base[pos] = e
	}
	// Both the surviving shared entries and the additions are sorted; merge.
	added := stored[next:]
	merged := make([]Entry, 0, len(base)+len(added))
	for pos := range base {
		if deleted.has(pos) {
			continue
		}
		for len(added) > 0 && compareEntry(&added[0], &base[pos]) < 0 {
			merged = append(merged, added[0])
			added = added[1:]
		}
		merged = append(merged, base[pos])
	}
	idx.Entries = append(merged, added...)
	return nil
}
