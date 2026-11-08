package checkout

import (
	"bytes"
	"maps"
	"slices"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/store"
)

// outcome is what apply or pull did with one changed path.
type outcome int

// Merge outcomes; skipped means the target already had the incoming version.
const (
	skipped outcome = iota
	applied
	merged
	marked     // written with conflict markers for the user to resolve
	unresolved // left untouched: it still holds conflict markers
	conflict   // left untouched
)

// applySource keys the merge bases of apply (see checkoutMeta.MergeBases).
const applySource = "apply"

// pullSource keys the merge bases of pulls from the lane attached as tree from.
func pullSource(from store.OID) string { return "pull " + string(from) }

// sides names the two sides of a merge: where each changed, for conflict
// reasons, and how conflict markers label them.
type sides struct {
	ours, theirs string
	labels       git.Labels
}

// conflict says why a path both sides changed since base conflicts, and whether
// a text merge may still settle it.
func (s sides) conflict(base, ours, theirs store.File) (why string, text bool) {
	switch {
	case theirs.ID == "":
		return "deleted " + s.theirs + " but changed " + s.ours, false
	case ours.ID == "":
		return "changed " + s.theirs + " but deleted " + s.ours, false
	case base.ID == "":
		return "added in both with different content", regular(ours) && regular(theirs)
	}
	return "changed on both sides", regular(base) && regular(ours) && regular(theirs)
}

// regular reports whether a leaf is a file rather than a symlink or directory.
func regular(f store.File) bool { return f.Mode == store.ModeFile || f.Mode == store.ModeExec }

// mergeText merges a text file both sides changed. A clean merge comes back
// merged, or skipped when ours already holds it; overlapping changes come back
// marked, with the conflict markers to write. Ours still holding markers stays
// unresolved, and content git cannot merge stays a conflict, why explaining it.
func mergeText(labels git.Labels, ours, base, theirs []byte, why string) ([]byte, outcome, string) {
	if hasMarkers(ours) {
		return nil, unresolved, "conflict markers are not resolved yet"
	}
	data, clean, err := git.MergeFile(ours, base, theirs, labels)
	switch {
	case err != nil:
		return nil, conflict, why + " (" + err.Error() + ")"
	case !clean:
		return data, marked, why
	case bytes.Equal(ours, data):
		return nil, skipped, ""
	}
	return data, merged, ""
}

// hasMarkers reports whether data holds an unresolved conflict: a line opening
// one and a later line closing it, as git merge-file writes them.
func hasMarkers(data []byte) bool {
	open := false
	for line := range bytes.Lines(data) {
		switch {
		case bytes.HasPrefix(line, []byte("<<<<<<< ")):
			open = true
		case open && bytes.HasPrefix(line, []byte(">>>>>>> ")):
			return true
		}
	}
	return false
}

// report adds one path's outcome to a merge reply.
func report(reply *api.ApplyReply, c api.Change, o outcome, reason string) {
	switch o {
	case applied:
		reply.Applied = append(reply.Applied, c)
	case merged:
		reply.Merged = append(reply.Merged, c.Path)
	case marked, unresolved:
		reply.Conflicts = append(reply.Conflicts, api.Conflict{Path: c.Path, Reason: reason, Markers: true})
	case conflict:
		reply.Conflicts = append(reply.Conflicts, api.Conflict{Path: c.Path, Reason: reason})
	}
}

// reportPending reports the based paths a merge did not reach whose target
// still holds conflict markers, so they keep asking to be resolved.
func reportPending(reply *api.ApplyReply, bases map[string]store.File, seen map[string]bool, hasMarkersAt func(p string) (bool, error)) error {
	for _, p := range slices.Sorted(maps.Keys(bases)) {
		if seen[p] {
			continue
		}
		ok, err := hasMarkersAt(p)
		if err != nil {
			return err
		}
		if ok {
			report(reply, api.Change{Path: p}, unresolved, "conflict markers are not resolved yet")
		}
	}
	return nil
}

// advance keeps bases current once a merge settled p. A path marked now, or
// one marked earlier that the merge got past, takes theirs as its base: the
// target holds that version's changes, resolved by the user, so the next merge
// starts from it rather than merging them again.
func advance(bases map[string]store.File, p string, theirs store.File, o outcome) {
	_, based := bases[p]
	switch {
	case o == conflict || o == unresolved || !based && o != marked:
	case theirs.ID == "" || theirs.Mode == store.ModeDir:
		delete(bases, p)
	default:
		bases[p] = theirs
	}
}

// mergeBases returns the leaves that replace the base of source's merges into h.
func (r *Registry) mergeBases(h *checkoutRecord, source string) (map[string]store.File, error) {
	r.mu.RLock()
	id := h.meta.MergeBases[source]
	r.mu.RUnlock()
	if id == "" {
		return map[string]store.File{}, nil
	}
	return r.store.Flatten(store.OID(id))
}

// setMergeBases persists bases as the merge bases of source's merges into h,
// first storing with read any leaf content the store lacks.
func (r *Registry) setMergeBases(h *checkoutRecord, source string, bases map[string]store.File, read func(p string, f store.File) ([]byte, error)) error {
	id := ""
	if len(bases) != 0 {
		for p, f := range bases {
			if r.store.Has(f.ID) {
				continue
			}
			data, err := read(p, f)
			if err != nil {
				return err
			}
			if _, err := r.store.PutBytes(data); err != nil {
				return err
			}
		}
		tree, err := r.store.Build(bases)
		if err != nil {
			return err
		}
		id = string(tree)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return failure("io", "registry is closed")
	}
	old := h.meta.MergeBases[source]
	if id == old {
		return nil
	}
	set := func(id string) {
		if id == "" {
			delete(h.meta.MergeBases, source)
			return
		}
		if h.meta.MergeBases == nil {
			h.meta.MergeBases = map[string]string{}
		}
		h.meta.MergeBases[source] = id
	}
	set(id)
	if err := r.persistLocked(); err != nil {
		set(old)
		return err
	}
	return nil
}
