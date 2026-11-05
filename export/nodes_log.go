package export

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/internal/durable"
)

// nodeRecord describes an allocation, subtree rename, retirement, or counter watermark.
type nodeRecord struct {
	Op   string `json:"op"`
	ID   uint64 `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
	To   string `json:"to,omitempty"`
}

// openNodeTable replays complete records and atomically compacts the live identities.
func openNodeTable(name string) (*nodeTable, error) {
	t := newNodeTable()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		r := bufio.NewReader(f)
		for {
			line, readErr := r.ReadBytes('\n')
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				f.Close()
				return nil, readErr
			}
			var rec nodeRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				f.Close()
				return nil, err
			}
			switch rec.Op {
			case "alloc":
				dir, base := split(rec.Path)
				parent := t.find(dir)
				if parent == nil || rec.ID < 2 || t.byID[rec.ID] != nil {
					f.Close()
					return nil, errors.New("invalid node allocation log")
				}
				n := &node{id: rec.ID, name: base, parent: parent}
				if parent.kids == nil {
					parent.kids = make(map[string]*node)
				}
				parent.kids[base] = n
				t.byID[n.id] = n
				if t.next <= n.id {
					t.next = n.id + 1
				}
			case "rename":
				t.rename(rec.Path, rec.To)
			case "remove":
				t.remove(rec.Path)
			case "next":
				if rec.ID > t.next {
					t.next = rec.ID
				}
			default:
				f.Close()
				return nil, errors.New("unknown node log operation")
			}
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".nodes-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	ids := make([]uint64, 0, len(t.byID))
	for id := range t.byID {
		if id != 1 {
			ids = append(ids, id)
		}
	}
	// Parent identities always precede children, including after subtree moves.
	sort.Slice(ids, func(i, j int) bool {
		a, _ := t.path(ids[i])
		b, _ := t.path(ids[j])
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		parts, _ := t.path(id)
		if err := enc.Encode(nodeRecord{Op: "alloc", ID: id, Path: strings.Join(parts, "/")}); err != nil {
			tmp.Close()
			return nil, err
		}
	}
	if err := enc.Encode(nodeRecord{Op: "next", ID: t.next}); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := durable.Sync(tmp); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		return nil, err
	}
	if err := syncNodeDirectory(filepath.Dir(name)); err != nil {
		return nil, err
	}
	t.log, err = os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	go t.syncLoop()
	return t, nil
}

// record appends a whole operation while the node mutex is held.
func (t *nodeTable) record(r nodeRecord) {
	if t.log == nil || t.logErr != nil {
		return
	}
	b, err := json.Marshal(r)
	if err == nil {
		b = append(b, '\n')
		_, err = t.log.Write(b)
	}
	if err != nil {
		t.logErr = err
	} else {
		t.dirty = true
	}
}

// syncLoop groups durable flushes without retaining records in userspace
// buffers. It flushes outside the node mutex: an fsync (FlushFileBuffers on
// Windows, often milliseconds) must not stall every lookup and allocation.
// Records appended meanwhile mark the log dirty again for the next flush.
func (t *nodeTable) syncLoop() {
	defer close(t.done)
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			t.mu.Lock()
			dirty := t.dirty
			t.dirty = false
			t.mu.Unlock()
			if !dirty {
				continue
			}
			if err := durable.Sync(t.log); err != nil {
				t.mu.Lock()
				if t.logErr == nil {
					t.logErr = err
				}
				t.mu.Unlock()
			}
		case <-t.stop:
			return
		}
	}
}

// close stops the flush worker and makes every appended operation durable.
func (t *nodeTable) close() error {
	t.closeOnce.Do(func() {
		if t.stop != nil {
			close(t.stop)
			<-t.done
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.log != nil {
			if err := durable.Sync(t.log); err != nil && t.logErr == nil {
				t.logErr = err
			}
			if err := t.log.Close(); err != nil && t.logErr == nil {
				t.logErr = err
			}
		}
	})
	return t.logErr
}
