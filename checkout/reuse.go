package checkout

import (
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/source"
)

// resolveRoot returns the source root of folder dir as source.Resolve does,
// remembering it for a repository's folders: a repeated create resolves without
// running git while the root still holds its .git and no folder between dir and
// the root gained one. Concurrent creates of a folder wait for one resolution.
func (r *Registry) resolveRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		r.rootsMu.Lock()
		root, known := r.roots[abs]
		pending := r.resolving[abs]
		if !known && pending == nil {
			pending = make(chan struct{})
			r.resolving[abs] = pending
			r.rootsMu.Unlock()
			break
		}
		r.rootsMu.Unlock()
		if !known {
			<-pending
			continue
		}
		if exists(filepath.Join(root, ".git")) && !nestedGit(root, abs) {
			return root, nil
		}
		r.rootsMu.Lock()
		if r.roots[abs] == root {
			delete(r.roots, abs)
		}
		r.rootsMu.Unlock()
	}
	root, err := source.Resolve(abs)
	r.rootsMu.Lock()
	if err == nil && exists(filepath.Join(root, ".git")) {
		r.roots[abs] = root
	}
	close(r.resolving[abs])
	delete(r.resolving, abs)
	r.rootsMu.Unlock()
	return root, err
}

// nestedGit reports whether a folder from dir up to, but not including, root holds
// a .git entry.
func nestedGit(root, dir string) bool {
	for inside(root, dir) && filepath.Clean(dir) != filepath.Clean(root) {
		if exists(filepath.Join(dir, ".git")) {
			return true
		}
		dir = filepath.Dir(dir)
	}
	return false
}

// inside reports whether p is dir or lies below it.
func inside(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
