package smb

import (
	"encoding/binary"
	"math"
	"sync"
)

// byteRange identifies an SMB lock and whether competing readers are allowed.
type byteRange struct {
	offset, length uint64
	exclusive      bool
}

// lockElement is one validated lock or exact-range unlock in a LOCK request.
type lockElement struct {
	byteRange
	unlock bool
}

// lockManager holds server-wide mandatory byte-range locks and a change broadcast.
type lockManager struct {
	mu      sync.Mutex
	held    map[*open][]byteRange
	changed chan struct{}
}

// init registers SMB byte-range locking.
func init() { handlers[cmdLock] = handleLock }

// overlap compares half-open ranges without overflowing the offset arithmetic.
func overlap(a, al, b, bl uint64) bool {
	if al == 0 || bl == 0 {
		return false
	}
	if a <= b {
		return b-a < al
	}
	return a-b < bl
}

// wakeLocked broadcasts a committed lock-table change while mu is held.
func (m *lockManager) wakeLocked() {
	if m.changed != nil {
		close(m.changed)
	}
	m.changed = make(chan struct{})
}

// releaseAll drops the locks of a closed handle and wakes competing requests.
func (m *lockManager) releaseAll(o *open) {
	m.mu.Lock()
	delete(m.held, o)
	m.wakeLocked()
	m.mu.Unlock()
}

// conflicts enforces mandatory locks for I/O issued through a different handle.
func (m *lockManager) conflicts(o *open, offset, length uint64, write bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	o.mu.Lock()
	p := o.path
	o.mu.Unlock()
	for other, locks := range m.held {
		if other == o {
			continue
		}
		other.mu.Lock()
		same := other.path == p
		other.mu.Unlock()
		if !same {
			continue
		}
		for _, l := range locks {
			if overlap(offset, length, l.offset, l.length) && (write || l.exclusive) {
				return true
			}
		}
	}
	return false
}

// try atomically validates and commits every element, returning a wake channel on contention.
func (m *lockManager) try(o *open, elements []lockElement) (uint32, <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held == nil {
		m.held = make(map[*open][]byteRange)
	}
	if m.changed == nil {
		m.changed = make(chan struct{})
	}
	current := append([]byteRange(nil), m.held[o]...)
	o.mu.Lock()
	p := o.path
	o.mu.Unlock()
	for _, e := range elements {
		if e.unlock {
			found := -1
			for i, l := range current {
				if l.offset == e.offset && l.length == e.length {
					found = i
					break
				}
			}
			if found == -1 {
				return statusRangeNotLocked, nil
			}
			current = append(current[:found], current[found+1:]...)
			continue
		}
		for _, l := range current {
			if overlap(e.offset, e.length, l.offset, l.length) && (e.exclusive || l.exclusive) {
				return statusLockNotGranted, m.changed
			}
		}
		for other, locks := range m.held {
			if other == o {
				continue
			}
			other.mu.Lock()
			same := other.path == p
			other.mu.Unlock()
			if !same {
				continue
			}
			for _, l := range locks {
				if overlap(e.offset, e.length, l.offset, l.length) && (e.exclusive || l.exclusive) {
					return statusLockNotGranted, m.changed
				}
			}
		}
		current = append(current, e.byteRange)
	}
	if len(current) == 0 {
		delete(m.held, o)
	} else {
		m.held[o] = current
	}
	m.wakeLocked()
	return statusSuccess, nil
}

// handleLock validates the lock vector and asynchronously waits when immediate failure is not requested.
func handleLock(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 48 || binary.LittleEndian.Uint16(b) != 48 {
		return nil, statusInvalidParameter
	}
	n := int(binary.LittleEndian.Uint16(b[2:]))
	if n == 0 || n > (len(b)-24)/24 {
		return nil, statusInvalidParameter
	}
	o, st := r.openFile(getOpenID(b[8:]))
	if st != statusSuccess {
		return nil, st
	}
	if o.isDir || o.pipe != nil {
		return nil, statusInvalidDeviceRequest
	}
	elements := make([]lockElement, n)
	immediate := false
	for i := range elements {
		v := b[24+i*24:]
		offset, length := binary.LittleEndian.Uint64(v), binary.LittleEndian.Uint64(v[8:])
		flags := binary.LittleEndian.Uint32(v[16:])
		kind := flags & 7
		if flags&^uint32(0x17) != 0 || (kind != 1 && kind != 2 && kind != 4) || (kind == 4 && flags&0x10 != 0) || length > math.MaxUint64-offset {
			return nil, statusInvalidParameter
		}
		if kind != 4 && o.access&(accessReadData|accessWriteData|accessAppendData) == 0 {
			return nil, statusAccessDenied
		}
		elements[i] = lockElement{byteRange{offset, length, kind == 2}, kind == 4}
		immediate = immediate || flags&0x10 != 0
	}
	m := &r.c.srv.st.locks
	st, wake := m.try(o, elements)
	if st == statusSuccess {
		if r.c.srv.st.opens.byID(o.id) != o {
			m.releaseAll(o)
			return nil, statusFileClosed
		}
		return []byte{4, 0, 0, 0}, st
	}
	if st != statusLockNotGranted || immediate {
		return nil, st
	}
	a := r.async()
	go func() {
		for {
			select {
			case <-a.cancelled:
				r.complete(a, nil, statusCancelled)
				return
			case <-wake:
			}
			if r.c.srv.st.opens.byID(o.id) != o {
				r.complete(a, nil, statusFileClosed)
				return
			}
			status, next := m.try(o, elements)
			if status == statusSuccess {
				if r.c.srv.st.opens.byID(o.id) != o {
					m.releaseAll(o)
					r.complete(a, nil, statusFileClosed)
					return
				}
				r.complete(a, []byte{4, 0, 0, 0}, statusSuccess)
				return
			}
			if status != statusLockNotGranted {
				r.complete(a, nil, status)
				return
			}
			wake = next
		}
	}()
	return nil, statusPending
}
