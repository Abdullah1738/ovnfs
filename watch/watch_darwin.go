//go:build darwin && cgo

package watch

/*
#include <stddef.h>
#include <stdint.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Watcher reports changes below added directories through one FSEvents
// stream; its methods are safe for concurrent use.
type Watcher struct {
	hub
	id uintptr // key in watchers, passed to FSEvents as the stream context
	q  queue

	mu     sync.Mutex        // serializes Add, Remove and Close
	dirs   map[string]string // Add spelling -> real path
	stream *stream
	resume uint64 // when no stream runs: id the next one starts after, or 0
	closed bool
	marker string // private directory every stream also watches, for barrier
	seq    int    // names barrier's marker files

	lastID  atomic.Uint64          // highest event id the stream delivered
	want    atomic.Pointer[string] // marker file barrier waits for
	reached chan struct{}          // signals that want was delivered; capacity 1
}

var (
	watchersMu sync.Mutex
	watchers   = map[uintptr]*Watcher{}
	watcherSeq uintptr
)

// New starts a watcher that calls fn with batches of changed paths; fn runs on
// one goroutine at a time and must not block for long or call Close.
func New(fn func(paths []string)) (*Watcher, error) {
	dir, err := os.MkdirTemp("", "ovnfs-watch-")
	if err != nil {
		return nil, err
	}
	marker, err := canonical(dir)
	if err != nil {
		os.Remove(dir)
		return nil, err
	}
	w := &Watcher{
		q:       newQueue(),
		dirs:    map[string]string{},
		marker:  marker,
		reached: make(chan struct{}, 1),
	}
	w.start(fn)
	watchersMu.Lock()
	watcherSeq++
	w.id = watcherSeq
	watchers[w.id] = w
	watchersMu.Unlock()
	return w, nil
}

// Add watches dir recursively; adding a watched dir again is a no-op.
func (w *Watcher) Add(dir string) error {
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
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("watch: watcher closed")
	}
	prev, had := w.dirs[dir]
	if had && prev == real {
		return nil
	}
	w.dirs[dir] = real
	if err := w.restart(); err != nil {
		if had {
			w.dirs[dir] = prev
		} else {
			delete(w.dirs, dir)
		}
		// Best effort: resume the previous set where it stopped.
		_ = w.restart()
		return err
	}
	return nil
}

// Remove stops watching dir; once it returns fn gets no more paths for dir
// beyond a call already in flight.
func (w *Watcher) Remove(dir string) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.dirs[dir]; !ok || w.closed {
		return
	}
	delete(w.dirs, dir)
	// On failure no stream runs until the next Add, which replays the events
	// missed meanwhile.
	_ = w.restart()
}

// Covers reports whether every change below dir, an added directory, now
// reaches fn: false while no stream runs.
func (w *Watcher) Covers(dir string) bool { return w.covers(dir) }

// Sync returns once every change made before the call has reached fn, or an
// error when no stream runs or the stream did not deliver a marker in time.
func (w *Watcher) Sync() error {
	w.mu.Lock()
	reached := !w.closed && w.stream != nil && w.barrier()
	w.mu.Unlock()
	if !reached || !w.drain() {
		return errors.New("watch: changes not delivered")
	}
	return nil
}

// Close stops the stream and returns once no fn call is in flight; fn is not
// called afterwards.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.exited
		return nil
	}
	w.closed = true
	if w.stream != nil {
		w.stream.stop()
		w.stream = nil
	}
	w.mu.Unlock()
	os.RemoveAll(w.marker)
	watchersMu.Lock()
	delete(watchers, w.id)
	watchersMu.Unlock()
	w.stop()
	w.q.release()
	return nil
}

// publish exposes w.dirs, each covered if the stream watches it, to
// callbacks; w.mu must be held.
func (w *Watcher) publish() {
	roots := make([]root, 0, len(w.dirs))
	for dir, real := range w.dirs {
		covered := w.stream != nil && slices.ContainsFunc(w.stream.paths, func(p string) bool { return below(p, real) })
		roots = append(roots, root{dir, real, covered})
	}
	w.roots.Store(&roots)
}

// restart replaces the stream with one covering w.dirs; w.mu must be held.
// The new stream replays FSEvents' history from the last event the old one
// delivered, so directories watched by both miss no event: neither a flush
// before stopping nor starting from the current event id catches events the
// kernel queued but fseventsd had not yet passed on.
func (w *Watcher) restart() error {
	w.publish()
	if w.stream != nil {
		w.barrier()
		w.stream.stop()
		w.stream = nil
		w.resume = w.lastID.Load()
	}
	if len(w.dirs) == 0 {
		w.resume = 0
		return nil
	}
	reals := []string{w.marker}
	for _, real := range w.dirs {
		reals = append(reals, real)
	}
	since := w.resume
	if since == 0 {
		since = now()
	}
	s, err := startStream(w.id, outermost(reals), w.q, since)
	if err != nil {
		w.resume = since
		w.publish()
		return err
	}
	w.lastID.Store(since)
	w.stream = s
	w.resume = 0
	w.publish()
	return nil
}

// barrier waits briefly until the stream has delivered every event older
// than the call, by writing a marker file and waiting for its event; FSEvents
// delivers in event id order. It reports whether the marker arrived in time.
// Besides Sync, restart uses it to keep the next stream's replay short however
// long the old stream sat idle; w.mu must be held.
func (w *Watcher) barrier() bool {
	w.seq++
	p := filepath.Join(w.marker, strconv.Itoa(w.seq))
	select {
	case <-w.reached:
	default:
	}
	w.want.Store(&p)
	defer w.want.Store(nil)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		return false
	}
	defer os.Remove(p)
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case <-w.reached:
		return true
	case <-t.C:
		return false
	}
}

//export watchCallback
func watchCallback(ctx C.uintptr_t, n C.size_t, paths **C.char, flags *C.uint32_t, ids *C.uint64_t) {
	watchersMu.Lock()
	w := watchers[uintptr(ctx)]
	watchersMu.Unlock()
	if w == nil || n == 0 {
		return
	}
	ps := unsafe.Slice(paths, int(n))
	fs := unsafe.Slice(flags, int(n))
	evs := make([]event, 0, n)
	want := w.want.Load()
	for i, id := range unsafe.Slice(ids, int(n)) {
		// Callbacks run one at a time, so a plain max suffices.
		if uint64(id) > w.lastID.Load() {
			w.lastID.Store(uint64(id))
		}
		path, f := C.GoString(ps[i]), uint32(fs[i])
		if f&flagDropped == 0 && below(w.marker, path) {
			if want != nil && path == *want {
				select {
				case w.reached <- struct{}{}:
				default:
				}
			}
			continue
		}
		switch {
		case f&flagDropped != 0:
			evs = append(evs, event{kind: dropped})
		case f&flagScan != 0:
			evs = append(evs, event{filepath.Clean(path), rescan})
		case f&flagNoPath == 0:
			evs = append(evs, event{filepath.Clean(path), changed})
		}
	}
	w.report(evs)
}
