//go:build windows

package watch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// filter selects the changes ReadDirectoryChangesW reports.
const filter = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_ATTRIBUTES | windows.FILE_NOTIFY_CHANGE_SIZE |
	windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_CREATION |
	windows.FILE_NOTIFY_CHANGE_SECURITY

// bufSize is the record buffer per top: the most network shares accept.
const bufSize = 64 << 10

// Watcher reports changes below added directories through one recursive
// ReadDirectoryChangesW request per top (an outermost added directory), all
// completing on one port; its methods are safe for concurrent use.
//
// A top is lost while it misses changes: after its directory was deleted, or
// renamed (noticed with the next change below it), or its handle failed.
type Watcher struct {
	forest[*top]
	port windows.Handle // completion port of every top's handle

	// Guarded by forest.mu:
	keys  map[uintptr]*top                  // tops with an open handle, by completion key
	next  uintptr                           // the last completion key handed out
	err   error                             // why waiting stopped, once it did
	syncs map[*windows.Overlapped]chan bool // barriers posted by Sync, answered true once reached

	reader chan struct{} // closed when wait returns
}

// syncKey marks the completion packets Sync posts; tops' keys count up from 1.
const syncKey = ^uintptr(0)

// top is a directory handle with its change request.
type top struct {
	real string
	h    windows.Handle
	key  uintptr
	ov   windows.Overlapped
	buf  []byte // the system writes records here while busy
	busy bool   // a request is in flight; buf and ov must stay put
	stop bool   // dropped: release once the request completes
}

// New starts a watcher that calls fn with batches of changes; fn runs on
// one goroutine at a time and must not block for long or call Close.
func New(fn func(changes []Change)) (*Watcher, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return nil, os.NewSyscallError("CreateIoCompletionPort", err)
	}
	w := &Watcher{port: port, keys: map[uintptr]*top{}, syncs: map[*windows.Overlapped]chan bool{}, reader: make(chan struct{})}
	w.init(w, fn)
	go w.wait()
	return w, nil
}

// Add watches dir recursively; adding a watched dir again is a no-op.
func (w *Watcher) Add(dir string) error { return w.add(dir) }

// Remove stops watching dir; once it returns fn gets no more paths for dir
// beyond a call already in flight.
func (w *Watcher) Remove(dir string) { w.remove(dir) }

// Covers reports whether every change below dir, an added directory, now
// reaches fn.
func (w *Watcher) Covers(dir string) bool { return w.covers(dir) }

// Sync returns once every change made before the call has reached fn, or an
// error once the watcher stopped. A write through a handle still open counts
// once the handle closes: the system reports it only then. Completion packets
// leave the port in order: a barrier posted now follows the packets of every
// request a change already completed. Handling those re-arms their tops, and a
// top that changed while unarmed completes its new request at once, behind the
// barrier; a second barrier follows those.
func (w *Watcher) Sync() error {
	for range 2 {
		ov := new(windows.Overlapped)
		reached := make(chan bool, 1)
		w.mu.Lock()
		if w.closed || w.err != nil {
			w.mu.Unlock()
			return errors.New("watch: watcher stopped")
		}
		w.syncs[ov] = reached
		w.mu.Unlock()
		if err := windows.PostQueuedCompletionStatus(w.port, 0, syncKey, ov); err != nil {
			w.mu.Lock()
			delete(w.syncs, ov)
			w.mu.Unlock()
			return os.NewSyscallError("PostQueuedCompletionStatus", err)
		}
		if !<-reached {
			return errors.New("watch: watcher stopped")
		}
	}
	if !w.drain() {
		return errors.New("watch: watcher stopped")
	}
	return nil
}

// Close stops watching and returns once no fn call is in flight; fn is not
// called afterwards.
func (w *Watcher) Close() error {
	if !w.shut() {
		<-w.exited
		return nil
	}
	w.mu.Lock()
	for _, t := range w.keys {
		w.drop(t)
	}
	w.mu.Unlock()
	// Wakes wait should no request be left to complete.
	_ = windows.PostQueuedCompletionStatus(w.port, 0, 0, nil)
	<-w.reader
	windows.CloseHandle(w.port)
	w.stop()
	return nil
}

// canonical returns the final path of the directory at path: links resolved,
// short names expanded, letter case as stored.
func canonical(path string) (string, error) {
	h, err := open(path)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	return finalPath(h, path)
}

// open opens the directory at path for overlapped change requests, letting
// others delete and rename it.
func open(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return windows.InvalidHandle, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return h, nil
}

// finalPath returns the path of h's file, opened as path, in DOS form.
func finalPath(h windows.Handle, path string) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", &os.PathError{Op: "GetFinalPathNameByHandle", Path: path, Err: err}
		}
		if n < uint32(len(buf)) {
			p := windows.UTF16ToString(buf[:n])
			if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
				return `\\` + rest, nil
			}
			return strings.TrimPrefix(p, `\\?\`), nil
		}
		buf = make([]uint16, n)
	}
}

// watch implements backend.
func (w *Watcher) watch(real string) (*top, error) {
	if w.err != nil {
		return nil, w.err
	}
	h, err := open(real)
	if err != nil {
		return nil, err
	}
	w.next++
	t := &top{real: real, h: h, key: w.next, buf: make([]byte, bufSize)}
	if _, err := windows.CreateIoCompletionPort(h, w.port, t.key, 0); err != nil {
		windows.CloseHandle(h)
		return nil, os.NewSyscallError("CreateIoCompletionPort", err)
	}
	w.keys[t.key] = t
	if err := w.arm(t); err != nil {
		w.drop(t)
		return nil, err
	}
	return t, nil
}

// drop implements backend; a request in flight is cancelled and t released
// once it completes.
func (w *Watcher) drop(t *top) {
	if t.busy {
		t.stop = true
		_ = windows.CancelIoEx(t.h, &t.ov)
		return
	}
	if w.keys[t.key] == t {
		delete(w.keys, t.key)
		windows.CloseHandle(t.h)
	}
}

// arm requests t's next changes; the system queues changes between requests.
func (w *Watcher) arm(t *top) error {
	t.ov = windows.Overlapped{}
	if err := windows.ReadDirectoryChanges(t.h, &t.buf[0], uint32(len(t.buf)), true, filter, nil, &t.ov, 0); err != nil {
		return &os.PathError{Op: "ReadDirectoryChangesW", Path: t.real, Err: err}
	}
	t.busy = true
	return nil
}

// wait handles completed requests until Close has released every top.
func (w *Watcher) wait() {
	defer func() {
		w.mu.Lock()
		for ov, reached := range w.syncs {
			delete(w.syncs, ov)
			reached <- false
		}
		w.mu.Unlock()
		close(w.reader)
	}()
	for {
		var n uint32
		var key uintptr
		var ov *windows.Overlapped
		err := windows.GetQueuedCompletionStatus(w.port, &n, &key, &ov, windows.INFINITE)
		w.mu.Lock()
		if ov == nil && err != nil {
			// Not seen in practice; nothing is covered from here on.
			w.err = fmt.Errorf("watch: wait for changes: %w", os.NewSyscallError("GetQueuedCompletionStatus", err))
			for k := range w.tops {
				w.lose(k)
			}
			w.mu.Unlock()
			return
		}
		if reached := w.syncs[ov]; key == syncKey && reached != nil {
			delete(w.syncs, ov)
			reached <- true
		} else if t := w.keys[key]; ov != nil && t != nil {
			w.complete(t, n, err)
		}
		done := w.closed && len(w.keys) == 0
		w.mu.Unlock()
		if done {
			return
		}
	}
}

// complete handles t's request completing with n bytes of records or err;
// w.mu is held.
func (w *Watcher) complete(t *top, n uint32, err error) {
	t.busy = false
	if t.stop {
		w.drop(t)
		return
	}
	var evs []event
	switch {
	case err == nil && n > 0:
		evs = parse(t.real, t.buf[:n])
	case err == nil, errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR):
		// The records overflowed the buffer.
		evs = []event{{t.real, rescan}}
	default:
		// The directory was deleted, or the handle failed otherwise.
		w.fail(t)
		return
	}
	// A renamed top no longer covers its own path.
	if p, err := finalPath(t.h, t.real); err != nil || !samePath(p, t.real) {
		w.report(evs)
		w.fail(t)
		return
	}
	if err := w.arm(t); err != nil {
		w.report(evs)
		w.fail(t)
		return
	}
	w.report(evs)
}

// fail gives up t, losing its top if t still serves it; w.mu is held.
func (w *Watcher) fail(t *top) {
	if w.tops[t.real] == t {
		w.lose(t.real)
	} else {
		w.drop(t)
	}
}

// parse returns the changed paths in buf's FILE_NOTIFY_INFORMATION records,
// whose names are relative to dir.
func parse(dir string, buf []byte) []event {
	var evs []event
	for len(buf) >= 12 {
		next := binary.LittleEndian.Uint32(buf)
		size := int(binary.LittleEndian.Uint32(buf[8:]))
		if 12+size > len(buf) {
			break
		}
		// The system aligns records to 4 bytes, so the name is aligned to 2.
		name := windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[12])), size/2))
		evs = append(evs, expand(dir, name))
		if next == 0 || int(next) > len(buf) {
			break
		}
		buf = buf[next:]
	}
	return evs
}

// expand returns the event for name, changed below dir. The system reports
// names as the changing call spelled them, which may use 8.3 short names;
// those are replaced by long ones, or, for entries gone meanwhile, stand for
// unknown changes in the directory holding the first short name.
func expand(dir, name string) event {
	p := filepath.Join(dir, name)
	known, ok := longPrefix(name)
	if ok {
		return event{p, changed}
	}
	if long, err := longPath(p); err == nil {
		return event{long, changed}
	}
	return event{filepath.Join(dir, known), rescan}
}

// longPrefix returns the components of name before the first that looks like
// an 8.3 short name (PROGRA~1, GITIGN~1.TXT), which long names hardly ever do,
// and whether name has none.
func longPrefix(name string) (string, bool) {
	var known []string
	for c := range strings.SplitSeq(name, `\`) {
		if short(c) {
			return filepath.Join(known...), false
		}
		known = append(known, c)
	}
	return name, true
}

// short reports whether path component c looks like an 8.3 short name.
func short(c string) bool {
	base, ext, _ := strings.Cut(c, ".")
	i := strings.LastIndexByte(base, '~')
	if i < 1 || i == len(base)-1 || len(base) > 8 || len(ext) > 3 || strings.Contains(ext, ".") || c != strings.ToUpper(c) {
		return false
	}
	return strings.Trim(base[i+1:], "0123456789") == ""
}

// longPath returns p with short names replaced by long ones.
func longPath(p string) (string, error) {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetLongPathName(u, &buf[0], uint32(len(buf)))
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n)
	}
}
