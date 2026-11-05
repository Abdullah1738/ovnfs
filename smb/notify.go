package smb

import (
	"encoding/binary"
	"strings"
	"sync"
)

// Notification actions identify the change represented by a FILE_NOTIFY_INFORMATION record.
const (
	notifyAdded      uint32 = 1
	notifyRemoved    uint32 = 2
	notifyModified   uint32 = 3
	notifyRenamedOld uint32 = 4
	notifyRenamedNew uint32 = 5
)

// notifyHub routes server mutations to directory opens without requiring host filesystem watchers.
type notifyHub struct {
	mu      sync.Mutex
	watches map[*open]*notifyWatch
}

// notifyWatch preserves a directory open's first notification filter and its buffered changes.
type notifyWatch struct {
	o        *open
	fs       Backend
	filter   uint32
	tree     bool
	queue    []notifyEvent
	bytes    int
	overflow bool
	closed   bool
	wake     chan struct{}
}

// notifyEvent is a relative Windows filename paired with its notification action.
type notifyEvent struct {
	name   string
	action uint32
}

// init registers asynchronous directory change notifications.
func init() { handlers[cmdChangeNotify] = handleChangeNotify }

// handleChangeNotify registers a watch before sending the pending response to avoid lost mutations.
func handleChangeNotify(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 32 || binary.LittleEndian.Uint16(b) != 32 {
		return nil, statusInvalidParameter
	}
	flags := binary.LittleEndian.Uint16(b[2:])
	filter := binary.LittleEndian.Uint32(b[24:])
	limit := binary.LittleEndian.Uint32(b[4:])
	max := r.c.maxTrans
	if max == 0 {
		max = 1 << 20
	}
	if flags & ^uint16(1) != 0 || filter == 0 || filter & ^uint32(0x0fff) != 0 || limit > max {
		return nil, statusInvalidParameter
	}
	if limit == 0 {
		return nil, statusBufferTooSmall
	}
	o, st := r.openFile(fileID{binary.LittleEndian.Uint64(b[8:]), binary.LittleEndian.Uint64(b[16:])})
	if st != statusSuccess {
		return nil, st
	}
	if !o.isDir {
		return nil, statusInvalidParameter
	}
	if o.access&1 == 0 {
		return nil, statusAccessDenied
	}
	h := &r.c.srv.st.notify
	h.mu.Lock()
	if h.watches == nil {
		h.watches = make(map[*open]*notifyWatch)
	}
	w := h.watches[o]
	if w == nil {
		w = &notifyWatch{o: o, fs: r.c.srv.FS, filter: filter, tree: flags&1 != 0, wake: make(chan struct{})}
		h.watches[o] = w
	}
	h.mu.Unlock()
	// Recheck the handle after registration so CLOSE cannot leave an orphan watch.
	if o.tree != nil && o.tree.open(o.id) != o {
		h.cancelOpen(o)
		return nil, statusNotifyCleanup
	}
	a := r.async()
	go h.awaitNotify(r, a, w, int(limit))
	return nil, statusPending
}

// awaitNotify consumes one batch, preserving the watch while no request is outstanding.
func (h *notifyHub) awaitNotify(r *request, a *asyncOp, w *notifyWatch, limit int) {
	for {
		h.mu.Lock()
		if w.closed {
			h.mu.Unlock()
			r.complete(a, nil, statusNotifyCleanup)
			return
		}
		select {
		case <-a.cancelled:
			h.mu.Unlock()
			r.complete(a, nil, statusCancelled)
			return
		default:
		}
		if w.overflow || len(w.queue) != 0 {
			var data []byte
			st := uint32(statusSuccess)
			if w.overflow {
				st = statusNotifyEnumDir
			} else {
				data = appendNotifyEvents(w.queue)
				if len(data) > limit {
					data, st = nil, statusNotifyEnumDir
				}
			}
			clear(w.queue)
			w.queue = w.queue[:0]
			w.bytes, w.overflow = 0, false
			h.mu.Unlock()
			if st != statusSuccess {
				r.complete(a, nil, st)
			} else {
				r.complete(a, directoryResponse(data), st)
			}
			return
		}
		wake := w.wake
		h.mu.Unlock()
		select {
		case <-wake:
		case <-a.cancelled:
		}
	}
}

// changed supplies conservative filters for callers that know only an action.
func (h *notifyHub) changed(p string, action uint32) {
	filter := uint32(3)
	if action == notifyModified {
		filter = 0x1fc
	}
	h.changedFiltered(p, action, filter)
}

// changedFiltered routes a mutation using its precise FILE_NOTIFY_CHANGE category mask.
func (h *notifyHub) changedFiltered(p string, action uint32, filter uint32) {
	if action < notifyAdded || action > notifyRenamedNew {
		return
	}
	h.mu.Lock()
	watches := make([]*notifyWatch, 0, len(h.watches))
	for _, w := range h.watches {
		watches = append(watches, w)
	}
	h.mu.Unlock()
	for _, w := range watches {
		if w.filter&filter == 0 {
			continue
		}
		w.o.mu.Lock()
		dir := w.o.path
		w.o.mu.Unlock()
		name := p
		if dir != "" {
			var ok bool
			name, ok = strings.CutPrefix(p, dir+"/")
			if !ok {
				continue
			}
		}
		if name == "" || !w.tree && strings.Contains(name, "/") {
			continue
		}
		// Resolve file-vs-directory categories when the mutation still has a name.
		if filter == 3 {
			if info, err := w.fs.Lstat(p); err == nil {
				category := uint32(1)
				if info.IsDir() {
					category = 2
				}
				if w.filter&category == 0 {
					continue
				}
			}
		}
		name = strings.ReplaceAll(name, "/", "\\")
		size := (12 + utf16Len(name) + 3) &^ 3
		h.mu.Lock()
		if !w.closed && !w.overflow {
			if len(w.queue) >= 1024 || size > (64<<10)-w.bytes {
				clear(w.queue)
				w.queue = w.queue[:0]
				w.bytes, w.overflow = 0, true
			} else {
				w.queue = append(w.queue, notifyEvent{name: name, action: action})
				w.bytes += size
			}
			close(w.wake)
			w.wake = make(chan struct{})
		}
		h.mu.Unlock()
	}
}

// cancelOpen closes all pending notifications and releases buffered state on handle teardown.
func (h *notifyHub) cancelOpen(o *open) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w := h.watches[o]; w != nil {
		delete(h.watches, o)
		w.closed = true
		clear(w.queue)
		w.queue = nil
		close(w.wake)
	}
}

// appendNotifyEvents encodes UTF-16 relative names in four-byte-aligned FSCC records.
func appendNotifyEvents(events []notifyEvent) []byte {
	var dst []byte
	previous := -1
	for _, e := range events {
		start := (len(dst) + 3) &^ 3
		if previous >= 0 {
			binary.LittleEndian.PutUint32(dst[previous:], uint32(start-previous))
		}
		dst = append(dst, make([]byte, start-len(dst))...)
		previous = start
		dst = binary.LittleEndian.AppendUint32(dst, 0)
		dst = binary.LittleEndian.AppendUint32(dst, e.action)
		dst = binary.LittleEndian.AppendUint32(dst, uint32(utf16Len(e.name)))
		dst = utf16Append(dst, e.name)
	}
	return dst
}
