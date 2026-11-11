//go:build linux || windows

package watch

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Backoff between attempts to rewatch lost tops.
const (
	minRetry = 100 * time.Millisecond
	maxRetry = 10 * time.Second
)

// backend watches the tops of a forest.
type backend[T comparable] interface {
	// watch starts reporting every change below real, the canonical path of a
	// directory; on failure it returns the zero T and leaves nothing watched.
	watch(real string) (T, error)
	// drop stops watching a top that watch returned.
	drop(t T)
}

// forest is the Watcher state shared by backends that watch each top (an
// outermost real path of the added dirs) on its own. A top whose watch breaks
// is lost: it is reported wholly changed, stops covering its dirs, and is
// rewatched with backoff, then reported wholly changed again.
type forest[T comparable] struct {
	hub
	be backend[T]

	mu     sync.Mutex        // guards the fields below and the backend's own state
	dirs   map[string]string // Add spelling -> real path
	tops   map[string]T      // real path -> its watch, zero while lost
	retry  *time.Timer       // rewatches lost tops; nil when none is due
	delay  time.Duration     // wait before the next retry
	closed bool
}

func (f *forest[T]) init(be backend[T], fn func(changes []Change)) {
	f.be = be
	f.dirs = map[string]string{}
	f.tops = map[string]T{}
	f.start(fn)
}

// add implements Watcher.Add.
func (f *forest[T]) add(dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	real, err := canonical(dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(real); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("watch: %s is not a directory", dir)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("watch: watcher closed")
	}
	if prev, had := f.dirs[dir]; had {
		if prev == real {
			return nil
		}
		// The spelling now leads elsewhere.
		delete(f.dirs, dir)
		_ = f.sync("")
	}
	f.dirs[dir] = real
	if err = f.sync(real); err != nil {
		delete(f.dirs, dir)
	}
	f.publish()
	return err
}

// remove implements Watcher.Remove.
func (f *forest[T]) remove(dir string) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.dirs[dir]; !ok || f.closed {
		return
	}
	delete(f.dirs, dir)
	_ = f.sync("")
	f.publish()
}

// shut marks f closed and stops retries, reporting false if it already was.
func (f *forest[T]) shut() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.closed = true
	if f.retry != nil {
		f.retry.Stop()
		f.retry = nil
	}
	return true
}

// sync makes the tops the outermost real paths of f.dirs, keeping the watches
// of tops that stay and starting new ones before dropping old ones, so a
// directory passing to another top misses no change. A new top that cannot be
// watched is lost, unless it holds must: then sync undoes its changes and
// returns the error. f.mu is held.
func (f *forest[T]) sync(must string) error {
	var zero T
	tops := make(map[string]T)
	var fresh []T
	for _, k := range outermost(slices.Collect(maps.Values(f.dirs))) {
		if t, ok := f.tops[k]; ok {
			tops[k] = t
			continue
		}
		t, err := f.be.watch(k)
		switch {
		case err == nil:
			fresh = append(fresh, t)
		case below(k, must):
			for _, t := range fresh {
				f.be.drop(t)
			}
			return err
		}
		tops[k] = t
	}
	for k, t := range f.tops {
		if t != zero && tops[k] != t {
			f.be.drop(t)
		}
	}
	f.tops = tops
	if slices.Contains(slices.Collect(maps.Values(tops)), zero) {
		f.schedule()
	}
	return nil
}

// publish exposes f.dirs and their coverage to the hub; f.mu is held.
func (f *forest[T]) publish() {
	var zero T
	roots := make([]root, 0, len(f.dirs))
	for dir, real := range f.dirs {
		covered := false
		for k, t := range f.tops {
			if t != zero && below(k, real) {
				covered = true
				break
			}
		}
		roots = append(roots, root{dir, real, covered})
	}
	f.roots.Store(&roots)
}

// lose drops top k after its watch stopped seeing every change below it,
// reports it wholly changed and schedules its rewatch; f.mu is held.
func (f *forest[T]) lose(k string) {
	var zero T
	t := f.tops[k]
	if t == zero {
		return
	}
	f.tops[k] = zero
	f.be.drop(t)
	f.publish()
	f.report([]event{{k, rescan}})
	f.schedule()
}

// schedule arranges a retry of the lost tops; f.mu is held.
func (f *forest[T]) schedule() {
	if f.retry != nil || f.closed {
		return
	}
	f.delay = min(max(2*f.delay, minRetry), maxRetry)
	f.retry = time.AfterFunc(f.delay, f.rewatch)
}

// rewatch retries the lost tops, reporting those it recovers wholly changed:
// their changes went unseen meanwhile.
func (f *forest[T]) rewatch() {
	var zero T
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retry = nil
	if f.closed {
		return
	}
	lost := false
	for k, t := range f.tops {
		if t != zero {
			continue
		}
		t, err := f.be.watch(k)
		if err != nil {
			lost = true
			continue
		}
		f.tops[k] = t
		f.report([]event{{k, rescan}})
	}
	f.publish()
	if lost {
		f.schedule()
	} else {
		f.delay = 0
	}
}
