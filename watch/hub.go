//go:build (darwin && cgo) || linux || windows

package watch

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// root is a watched directory as passed to Add and as the backend reports it.
type root struct {
	dir, real string
	covered   bool // the backend currently sees every change below real
}

// item is a Change, kept with the watched dir it was found below.
type item struct {
	dir, path string
	tree      bool
}

// kind is what a backend event tells about its path.
type kind uint8

const (
	changed kind = iota // the path itself changed
	rescan              // anything at or below the path may have changed
	dropped             // anything anywhere may have changed; the path is unused
)

// event is one backend record about a real (canonical) path.
type event struct {
	path string
	kind kind
}

// hub maps backend events to the watched dirs they concern and calls fn with
// them, in order, on one goroutine; every backend's Watcher embeds one.
type hub struct {
	fn    func(changes []Change)
	roots atomic.Pointer[[]root] // stored by the backend whenever dirs or coverage change

	mu        sync.Mutex
	pending   [][]item
	stopped   bool
	queued    uint64    // batches ever reported
	delivered uint64    // batches handed to fn and returned from it, or dropped
	drained   sync.Cond // on mu; broadcast as delivered grows or at stop

	wake   chan struct{} // nudges deliver; capacity 1
	done   chan struct{} // closed by stop
	exited chan struct{} // closed when deliver returns
}

// start begins delivering to fn.
func (h *hub) start(fn func(changes []Change)) {
	h.fn = fn
	h.drained.L = &h.mu
	h.wake = make(chan struct{}, 1)
	h.done = make(chan struct{})
	h.exited = make(chan struct{})
	h.roots.Store(&[]root{})
	go h.deliver()
}

// drain returns once fn has returned for every batch reported before the call,
// reporting false if the hub stopped first.
func (h *hub) drain() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	target := h.queued
	for h.delivered < target && !h.stopped {
		h.drained.Wait()
	}
	return !h.stopped
}

// covers implements Watcher.Covers.
func (h *hub) covers(dir string) bool {
	dir, err := filepath.Abs(dir)
	return err == nil && slices.ContainsFunc(*h.roots.Load(), func(r root) bool { return r.dir == dir && r.covered })
}

// report hands evs, mapped to the watched dirs, to deliver without blocking
// the backend.
func (h *hub) report(evs []event) {
	b := batch(*h.roots.Load(), evs)
	if len(b) == 0 {
		return
	}
	h.mu.Lock()
	if !h.stopped {
		h.pending = append(h.pending, b)
		h.queued++
	}
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// stop discards pending batches and returns once no fn call is in flight; fn
// is not called afterwards.
func (h *hub) stop() {
	h.mu.Lock()
	h.stopped = true
	h.pending = nil
	h.drained.Broadcast()
	h.mu.Unlock()
	close(h.done)
	<-h.exited
}

// deliver calls fn with everything pending, merged into one batch, until stop:
// batches queued while fn runs cost one more call, not one each, and a tree
// change absorbs the changes below it.
func (h *hub) deliver() {
	defer close(h.exited)
	for {
		select {
		case <-h.wake:
		case <-h.done:
			return
		}
		for {
			h.mu.Lock()
			if h.stopped || len(h.pending) == 0 {
				h.mu.Unlock()
				break
			}
			batches := h.pending
			h.pending = nil
			h.mu.Unlock()
			if changes := h.live(slices.Concat(batches...)); len(changes) > 0 {
				h.fn(changes)
			}
			h.mu.Lock()
			h.delivered += uint64(len(batches))
			h.drained.Broadcast()
			h.mu.Unlock()
		}
	}
}

// live returns b's changes below directories still watched, without
// duplicates or changes a tree change covers.
func (h *hub) live(b []item) []Change {
	roots := *h.roots.Load()
	trees := map[string]bool{}
	for _, it := range b {
		if it.tree {
			trees[it.path] = true
		}
	}
	// covered reports whether a tree change at or above it.path (strictly
	// above for a tree change itself) includes it.
	covered := func(it item) bool {
		p := it.path
		if it.tree {
			if p == it.dir {
				return false
			}
			p = filepath.Dir(p)
		}
		for {
			if trees[p] {
				return true
			}
			if p == it.dir {
				return false
			}
			parent := filepath.Dir(p)
			if parent == p {
				return false
			}
			p = parent
		}
	}
	var changes []Change
	seen := map[string]bool{}
	for _, it := range b {
		if seen[it.path] || covered(it) || !slices.ContainsFunc(roots, func(r root) bool { return r.dir == it.dir }) {
			continue
		}
		seen[it.path] = true
		changes = append(changes, Change{Path: it.path, Tree: it.tree || trees[it.path]})
	}
	return changes
}

// batch maps events to changed paths spelled under the roots they lie below,
// without duplicates. A rescan below a root yields a tree change there; lost
// events, and rescans at or above a root, yield the roots they concern.
func batch(roots []root, evs []event) []item {
	var out []item
	seen := map[item]bool{}
	add := func(it item) {
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	all := func() {
		for _, r := range roots {
			add(item{r.dir, r.dir, true})
		}
	}
	for _, ev := range evs {
		switch ev.kind {
		case dropped:
			all()
		case rescan:
			hit := false
			for _, r := range roots {
				if rp, ok := rel(r.real, ev.path); ok {
					add(item{r.dir, filepath.Join(r.dir, rp), true})
					hit = true
				} else if below(ev.path, r.real) {
					add(item{r.dir, r.dir, true})
					hit = true
				}
			}
			if !hit {
				all()
			}
		default:
			for _, r := range roots {
				if rp, ok := rel(r.real, ev.path); ok {
					add(item{r.dir, filepath.Join(r.dir, rp), false})
				}
			}
		}
	}
	return out
}

// outermost returns the distinct paths that lie below no other of them, sorted.
func outermost(paths []string) []string {
	var out []string
	for i, p := range paths {
		inner := func(q string) bool { return below(q, p) && !below(p, q) }
		if !slices.ContainsFunc(paths[:i], func(q string) bool { return below(q, p) }) && !slices.ContainsFunc(paths[i+1:], inner) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// below reports whether p is real or lies under it.
func below(real, p string) bool {
	_, ok := rel(real, p)
	return ok
}

// rel returns p relative to real if p is real or lies under it.
func rel(real, p string) (string, bool) {
	if real == "" || len(p) < len(real) || !samePath(p[:len(real)], real) {
		return "", false
	}
	rest := p[len(real):]
	switch {
	case rest == "":
		return "", true
	case os.IsPathSeparator(real[len(real)-1]):
		return rest, true
	case os.IsPathSeparator(rest[0]):
		return rest[1:], true
	}
	return "", false
}

// samePath compares paths as the platform's filesystems usually do: ignoring
// case on Windows.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
