package checkout

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/internal/durable"
)

// stateVersion is the only registry manifest layout this build reads.
const stateVersion = 4

// diskState is the versioned registry manifest; all referenced paths live below State.
type diskState struct {
	Version      int            `json:"version"`
	Proto        string         `json:"proto"`
	Port         int            `json:"port"`
	NextSnapshot uint64         `json:"next_snapshot"`
	Bases        []base.Meta    `json:"bases"`
	Checkouts    []diskCheckout `json:"checkouts"`
	Snapshots    []diskSnapshot `json:"snapshots"`
	Layers       []diskLayer    `json:"layers"`
	Intents      []diskIntent   `json:"intents"`
}

// diskCheckout records a checkout; Head or Parent identifies its immutable stack.
type diskCheckout struct {
	checkoutMeta
	Stack []string `json:"stack,omitempty"`
}

// diskSnapshot records one layer and its snapshot ancestry; Stack is rebuilt at open.
type diskSnapshot struct {
	snapMeta
	Base        string   `json:"base"`
	Layer       string   `json:"layer,omitempty"`
	StackParent string   `json:"stack_parent,omitempty"`
	Dropped     bool     `json:"dropped,omitempty"`
	Stack       []string `json:"stack,omitempty"`
}

// diskLayer binds an indexed layer directory to its frozen base.
type diskLayer struct {
	ID, Base string
	Portable bool `json:"portable,omitempty"`
}

// diskIntent makes an interrupted multi-step filesystem mutation recoverable.
type diskIntent struct{ Op, Checkout, Layer, Snapshot string }

// stateLocked snapshots metadata while the registry mutex is held.
func (r *Registry) stateLocked() diskState {
	lineage := r.retainedLineageLocked()
	d := diskState{Version: stateVersion, Proto: r.proto, Port: r.port, NextSnapshot: r.nextSnapshot,
		Bases: []base.Meta{}, Checkouts: []diskCheckout{}, Snapshots: []diskSnapshot{}, Layers: []diskLayer{}, Intents: []diskIntent{}}
	for _, b := range r.bases {
		d.Bases = append(d.Bases, b.view.Meta())
	}
	for _, h := range r.checkouts {
		d.Checkouts = append(d.Checkouts, diskCheckout{checkoutMeta: cloneCheckoutMeta(h.meta), Stack: append([]string(nil), h.stack...)})
	}
	for _, s := range lineage {
		d.Snapshots = append(d.Snapshots, diskSnapshot{snapMeta: cloneSnapMeta(s.meta), Base: s.base.view.Meta().ID, Layer: s.layer, StackParent: s.stackParent, Dropped: s.dropped, Stack: append([]string(nil), s.stack...)})
	}
	for _, l := range r.layers {
		d.Layers = append(d.Layers, diskLayer{ID: l.id, Base: l.base, Portable: l.portable})
	}
	for _, intent := range r.intents {
		d.Intents = append(d.Intents, intent)
	}
	sort.Slice(d.Bases, func(i, j int) bool { return d.Bases[i].ID < d.Bases[j].ID })
	sort.Slice(d.Checkouts, func(i, j int) bool { return d.Checkouts[i].Name < d.Checkouts[j].Name })
	sort.Slice(d.Snapshots, func(i, j int) bool { return snapshotLess(d.Snapshots[i].ID, d.Snapshots[j].ID) })
	sort.Slice(d.Layers, func(i, j int) bool { return d.Layers[i].ID < d.Layers[j].ID })
	sort.Slice(d.Intents, func(i, j int) bool { return d.Intents[i].Checkout < d.Intents[j].Checkout })
	return d
}

// persistLocked commits all metadata with a file and directory durability barrier.
func (r *Registry) persistLocked() error {
	if err := writeState(r.config.State, r.stateLocked()); err != nil {
		return err
	}
	r.pruneLineageLocked()
	return nil
}

// persistIntentLocked publishes recovery metadata before filesystem changes without an extra durability barrier.
func (r *Registry) persistIntentLocked() error {
	return writeStateMode(r.config.State, r.stateLocked(), false)
}

// writeState atomically replaces state.json with a final process/kernel-crash barrier,
// not a power-loss guarantee; Darwin uses fsync(2), not F_FULLFSYNC.
func writeState(dir string, d diskState) error { return writeStateMode(dir, d, true) }

// writeStateMode keeps intermediate recovery snapshots atomic and syncs only the operation's final commit.
func writeStateMode(dir string, d diskState, syncCommit bool) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil && syncCommit {
		err = durable.Sync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(dir, "state.json")); err != nil {
		return err
	}
	if syncCommit {
		return durable.Dir(dir)
	}
	return nil
}

// exists reports whether a path survived an interrupted operation.
func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// recoverIntents finishes freezes and removals and rolls back unpublished creates.
func recoverIntents(state string, d *diskState) error {
	for _, intent := range d.Intents {
		hi, si := -1, -1
		for i := range d.Checkouts {
			if d.Checkouts[i].Name == intent.Checkout {
				hi = i
				break
			}
		}
		for i := range d.Snapshots {
			if d.Snapshots[i].ID == intent.Snapshot {
				si = i
				break
			}
		}
		upper := filepath.Join(state, "uppers", intent.Checkout)
		gitdir := filepath.Join(state, "git", intent.Checkout)
		switch intent.Op {
		case "creating":
			if hi >= 0 {
				continue
			}
			if err := os.RemoveAll(upper); err != nil {
				return err
			}
			if err := os.RemoveAll(gitdir); err != nil {
				return err
			}
			if intent.Layer != "" && !baseUsed(d, intent.Layer) {
				if err := os.RemoveAll(filepath.Join(state, "bases", intent.Layer)); err != nil {
					return err
				}
				kept := d.Bases[:0]
				for _, b := range d.Bases {
					if b.ID != intent.Layer {
						kept = append(kept, b)
					}
				}
				d.Bases = kept
			}
		case "freezing":
			layerDir := filepath.Join(state, "layers", intent.Layer)
			frozen := intent.Layer != "" && exists(layerDir)
			saved := hi >= 0 && (!d.Checkouts[hi].Git || exists(filepath.Join(state, "snapshots", intent.Snapshot, "git")))
			if hi >= 0 && si >= 0 && saved && (frozen || intent.Layer == "") {
				h := &d.Checkouts[hi]
				h.Head = copyString(&d.Snapshots[si].ID)
				h.Stack = append([]string(nil), d.Snapshots[si].Stack...)
				if frozen {
					if err := os.RemoveAll(filepath.Join(layerDir, ".wh..wh.work")); err != nil {
						return err
					}
					found := false
					for _, l := range d.Layers {
						found = found || l.ID == intent.Layer
					}
					if !found {
						d.Layers = append(d.Layers, diskLayer{ID: intent.Layer, Base: h.Base})
					}
				}
				if err := os.MkdirAll(upper, 0o755); err != nil {
					return err
				}
				if h.Git {
					if err := writeGitfile(upper, gitdir); err != nil {
						return err
					}
				}
			} else {
				if frozen && hi >= 0 {
					if err := os.RemoveAll(upper); err != nil {
						return err
					}
					if err := os.Rename(layerDir, upper); err != nil {
						return err
					}
				}
				if si >= 0 {
					d.Snapshots = append(d.Snapshots[:si], d.Snapshots[si+1:]...)
				}
				if err := os.RemoveAll(filepath.Join(state, "snapshots", intent.Snapshot)); err != nil {
					return err
				}
			}
		case "removing":
			if err := os.RemoveAll(gitdir); err != nil {
				return err
			}
			if err := os.RemoveAll(upper); err != nil {
				return err
			}
			if hi >= 0 {
				d.Checkouts = append(d.Checkouts[:hi], d.Checkouts[hi+1:]...)
			}
		default:
			return fmt.Errorf("unknown registry intent %q", intent.Op)
		}
	}
	d.Intents = nil
	return writeState(state, *d)
}

// baseUsed reports whether any checkout or snapshot still sits on a base.
func baseUsed(d *diskState, id string) bool {
	for _, h := range d.Checkouts {
		if h.Base == id {
			return true
		}
	}
	for _, s := range d.Snapshots {
		if s.Base == id {
			return true
		}
	}
	return false
}
