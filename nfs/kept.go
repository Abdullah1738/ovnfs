package nfs

import (
	"container/list"
	"io"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
)

// Kept-file limits.
const (
	// keptIdle is how long a kept file outlives its last use. NFSv3 has no
	// close, but clients send a file's cached writes when it is closed, so
	// the writes of one open come in a burst.
	keptIdle = 10 * time.Second
	// keptMax bounds the files kept at once; past it the least recently
	// used idle one is closed.
	keptMax = 4096
)

// keptFiles holds files opened for writing just before they lost their
// owner's write permission, by file id. A process may write through an open
// file it created read-only or chmodded read-only (git creates objects 0444
// and writes them), and NFS clients send those writes after the CREATE or
// SETATTR that took the permission away. The backend refuses to open such a
// file for writing again, as the local filesystem would; NFSv3 WRITE and
// size changes fall back to the kept file instead, as Linux's nfsd lets the
// owner write. NFSv4 keeps files per open instead (keepWritable).
type keptFiles struct {
	mu    sync.Mutex
	files map[uint64]*list.Element // of *keptFile
	lru   list.List                // most recently used first
	idle  time.Duration            // zero: keptIdle
}

// keptFile is one kept file. It closes once idle with no write under way.
type keptFile struct {
	id    uint64
	f     billy.File
	users int
	timer *time.Timer
}

// has reports whether id has a kept file.
func (k *keptFiles) has(id uint64) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.files[id]
	return ok
}

// keep holds f, open for writing, for id. f is closed instead when id
// already has a kept file.
func (k *keptFiles) keep(id uint64, f billy.File) {
	k.mu.Lock()
	if _, ok := k.files[id]; ok {
		k.mu.Unlock()
		f.Close()
		return
	}
	if k.files == nil {
		k.files = make(map[uint64]*list.Element)
	}
	var evicted billy.File
	if len(k.files) >= keptMax {
		evicted = k.evictLocked()
	}
	e := &keptFile{id: id, f: f}
	k.files[id] = k.lru.PushFront(e)
	e.timer = time.AfterFunc(k.idleTime(), func() { k.expire(e) })
	k.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}
}

// with runs fn on id's kept file; ok is false when there is none.
func (k *keptFiles) with(id uint64, fn func(billy.File) error) (ok bool, err error) {
	k.mu.Lock()
	el := k.files[id]
	if el == nil {
		k.mu.Unlock()
		return false, nil
	}
	e := el.Value.(*keptFile)
	e.users++
	k.lru.MoveToFront(el)
	k.mu.Unlock()

	err = fn(e.f)

	k.mu.Lock()
	e.users--
	e.timer.Reset(k.idleTime())
	k.mu.Unlock()
	return true, err
}

// expire closes e once it is idle.
func (k *keptFiles) expire(e *keptFile) {
	k.mu.Lock()
	el := k.files[e.id]
	if el == nil || el.Value != e {
		k.mu.Unlock()
		return
	}
	if e.users > 0 {
		e.timer.Reset(k.idleTime())
		k.mu.Unlock()
		return
	}
	k.lru.Remove(el)
	delete(k.files, e.id)
	k.mu.Unlock()
	e.f.Close()
}

// evictLocked drops the least recently used idle file and returns it for
// the caller to close outside the lock; nil when every file is in use.
func (k *keptFiles) evictLocked() billy.File {
	for el := k.lru.Back(); el != nil; el = el.Prev() {
		if e := el.Value.(*keptFile); e.users == 0 {
			e.timer.Stop()
			k.lru.Remove(el)
			delete(k.files, e.id)
			return e.f
		}
	}
	return nil
}

// idleTime is the configured idle lifetime.
func (k *keptFiles) idleTime() time.Duration {
	if k.idle > 0 {
		return k.idle
	}
	return keptIdle
}

// writeAt writes data at offset of f, without moving an offset others
// share where the file can.
func writeAt(f billy.File, data []byte, offset int64) (int, error) {
	if w, ok := f.(io.WriterAt); ok {
		return w.WriteAt(data, offset)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return f.Write(data)
}
