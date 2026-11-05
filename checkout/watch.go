package checkout

import (
	"path/filepath"
	"strings"
)

// Tracked reports whether every change to path p of checkout name reaches the
// daemon, so a file protocol may let clients cache it until recalled: false
// only for paths served live from a source folder the watcher does not cover
// right now.
func (r *Registry) Tracked(name, p string) bool {
	r.mu.RLock()
	h := r.checkouts[name]
	r.mu.RUnlock()
	if h == nil || h.retired.Load() {
		return false
	}
	if !h.base.view.Live(p) {
		return true
	}
	return r.watcher != nil && r.watcher.Covers(h.base.view.Meta().Dir)
}

// invalidate reports daemon-side changes to checkout paths (nil: anything in
// it) so the export can recall clients' cached copies. Callers hold no
// registry locks: the export reads the checkout to retire vanished paths.
func (r *Registry) invalidate(name string, paths []string) {
	if r.config.Invalidate != nil {
		r.config.Invalidate(name, paths)
	}
}

// wakeWatch asks the watch loop to resynchronize the watched folders after the
// set of bases changed.
func (r *Registry) wakeWatch() {
	select {
	case r.watchWake <- struct{}{}:
	default:
	}
}

// watchFolders keeps the folder watcher on exactly the source folders of local
// bases, off the registry lock: adding or removing a folder may wait for an
// in-flight event batch, which itself reads the registry.
func (r *Registry) watchFolders() {
	defer r.workers.Done()
	have := make(map[string]bool)
	for {
		select {
		case <-r.stop:
			return
		case <-r.watchWake:
		}
		r.mu.RLock()
		want := make(map[string]bool, len(r.bases))
		for _, b := range r.bases {
			if m := b.view.Meta(); m.Remote == nil && m.Dir != "" {
				want[m.Dir] = true
			}
		}
		r.mu.RUnlock()
		for dir := range have {
			if !want[dir] {
				r.watcher.Remove(dir)
				delete(have, dir)
			}
		}
		for dir := range want {
			if have[dir] {
				continue
			}
			if err := r.watcher.Add(dir); err != nil {
				r.enqueueNotice(baseNotice{fault: "watch " + dir + ": " + err.Error()})
				continue
			}
			have[dir] = true
		}
	}
}

// folderChanged maps folder edits to the checkouts whose live paths show them
// and invalidates those paths. A reported folder root means individual events
// were lost, so every checkout on that folder is invalidated whole.
func (r *Registry) folderChanged(paths []string) {
	changed := make(map[string][]string) // checkout -> changed paths, nil for all
	whole := make(map[string]bool)
	r.mu.RLock()
	users := make(map[*baseRecord][]string)
	for name, h := range r.checkouts {
		if !h.retired.Load() {
			users[h.base] = append(users[h.base], name)
		}
	}
	for b, names := range users {
		m := b.view.Meta()
		if m.Remote != nil || m.Dir == "" {
			continue
		}
		for _, p := range paths {
			rel, ok := below(m.Dir, p)
			if !ok {
				continue
			}
			for _, name := range names {
				switch {
				case rel == "":
					whole[name] = true
				case b.view.Live(rel):
					changed[name] = append(changed[name], rel)
				}
			}
		}
	}
	r.mu.RUnlock()
	for name := range whole {
		delete(changed, name)
		r.invalidate(name, nil)
	}
	for name, rels := range changed {
		r.invalidate(name, rels)
	}
}

// below returns p relative to dir in slash form ("" for dir itself), or false
// when p lies outside dir.
func below(dir, p string) (string, bool) {
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return filepath.ToSlash(rel), true
}
