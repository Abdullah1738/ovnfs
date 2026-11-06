package checkout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/store"
	"github.com/stencil-hq/ovnfs/watch"
)

// namePattern is the immutable exported folder-name grammar.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// objectPattern accepts the random on-disk base and layer identifiers.
var objectPattern = regexp.MustCompile(`^[bl]_[0-9a-f]{12}$`)

// stateDirs are the registry's subdirectories below State.
var stateDirs = []string{"bases", "layers", "uppers", "snapshots", "git", "sources"}

// lockFile holds a base's resolved manifest next to its capture.
const lockFile = "lock.json"

// gcGrace protects objects written by work still in flight from a concurrent store sweep.
const gcGrace = time.Hour

// Open restores the registry and reconciles durable filesystem mutation intents.
func Open(c Config) (*Registry, error) {
	if !filepath.IsAbs(c.State) || !filepath.IsAbs(c.Mount) {
		return nil, failure("invalid", "state and mount must be absolute paths")
	}
	c.State, c.Mount = filepath.Clean(c.State), filepath.Clean(c.Mount)
	if err := os.MkdirAll(c.State, 0o700); err != nil {
		return nil, coded(err)
	}
	for _, dir := range stateDirs {
		if err := os.MkdirAll(filepath.Join(c.State, dir), 0o700); err != nil {
			return nil, coded(err)
		}
	}
	stateLock, err := lockState(c.State)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			stateLock.Close()
		}
	}()
	d := diskState{Version: stateVersion}
	data, err := os.ReadFile(filepath.Join(c.State, "state.json"))
	if err == nil {
		if err = json.Unmarshal(data, &d); err != nil {
			return nil, coded(fmt.Errorf("read state.json: %w", err))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, coded(err)
	}
	if d.Version != stateVersion {
		return nil, failure("unsupported", "state in %s has version %d, this ovnfs reads only version %d; move it aside or use another --state", c.State, d.Version, stateVersion)
	}
	if err = validateDisk(d); err != nil {
		return nil, coded(err)
	}
	if len(d.Intents) != 0 {
		if err = recoverIntents(c.State, &d); err != nil {
			return nil, coded(err)
		}
	}
	if err = validateDisk(d); err != nil {
		return nil, coded(err)
	}
	if err = restoreStacks(&d); err != nil {
		return nil, coded(err)
	}
	st, err := store.Open(filepath.Join(c.State, "store"), c.BlobCache)
	if err != nil {
		return nil, coded(err)
	}
	r := &Registry{config: c, store: st, checkouts: make(map[string]*checkoutRecord), snapshots: make(map[string]*snapRecord), lineage: make(map[string]*snapRecord), bases: make(map[string]*baseRecord), layers: make(map[string]*layerRecord), intents: make(map[string]diskIntent), reserved: make(map[string]bool), nextSnapshot: d.NextSnapshot, proto: d.Proto, port: d.Port, rootTime: time.Now().UTC(), gcWake: make(chan struct{}, 1), noticeWake: make(chan struct{}, 1), stop: make(chan struct{})}
	r.stateLock = stateLock
	r.node = c.Node
	r.mounts = mountPaths(c.Mount)
	r.mirror = newMirror(filepath.Join(c.State, "mirror.git"), func(message string) { r.enqueueNotice(baseNotice{fault: message}) })
	cleanup := func() {
		for _, b := range r.bases {
			b.view.Close()
		}
		r.mirror.close()
	}
	for _, meta := range d.Bases {
		config := r.baseConfig(meta.ID)
		if meta.Remote != nil {
			config = r.remoteConfig(meta.ID, meta.Remote)
		}
		view, openErr := base.Open(config, meta)
		if openErr != nil {
			cleanup()
			return nil, coded(openErr)
		}
		b, openErr := loadBase(view, filepath.Join(c.State, "bases", meta.ID, lockFile))
		if openErr != nil {
			view.Close()
			cleanup()
			return nil, coded(openErr)
		}
		r.bases[meta.ID] = b
		if meta.Remote != nil {
			r.mirror.want(meta.Remote.Origins)
		}
	}
	for _, disk := range d.Layers {
		if r.bases[disk.Base] == nil && !disk.Portable {
			cleanup()
			return nil, failure("io", "layer %s references missing base %s", disk.ID, disk.Base)
		}
		layer, openErr := overlay.IndexLayer(filepath.Join(c.State, "layers", disk.ID))
		if openErr != nil {
			cleanup()
			return nil, coded(openErr)
		}
		r.layers[disk.ID] = &layerRecord{id: disk.ID, base: disk.Base, layer: layer, portable: disk.Portable}
	}
	for _, disk := range d.Checkouts {
		b := r.bases[disk.Base]
		if b == nil {
			cleanup()
			return nil, failure("io", "checkout %s references missing base %s", disk.Name, disk.Base)
		}
		stack, stackErr := r.layerStackLocked(disk.Stack, disk.Base)
		if stackErr != nil {
			cleanup()
			return nil, coded(stackErr)
		}
		upper := r.upperPath(disk.Name)
		if disk.Git {
			if err := git.MarkCheckout(context.Background(), r.gitdirPath(disk.Name), disk.Name); err != nil {
				cleanup()
				return nil, coded(err)
			}
		}
		fs, openErr := overlay.New(upper, stack, b.view.Bottom(), r.config.Memory)
		if openErr != nil {
			cleanup()
			return nil, coded(openErr)
		}
		h := &checkoutRecord{meta: disk.checkoutMeta, base: b, stack: append([]string{}, disk.Stack...), upper: upper, fs: fs}
		h.generation.Store(1)
		r.checkouts[disk.Name] = h
		r.retainLocked(b, h.stack)
	}
	for _, disk := range d.Snapshots {
		b := r.bases[disk.Base]
		if b == nil {
			cleanup()
			return nil, failure("io", "snapshot %s references missing base %s", disk.ID, disk.Base)
		}
		if _, stackErr := r.layerStackLocked(disk.Stack, disk.Base); stackErr != nil {
			cleanup()
			return nil, coded(stackErr)
		}
		s := &snapRecord{meta: disk.snapMeta, base: b, stack: append([]string{}, disk.Stack...), layer: disk.Layer, stackParent: disk.StackParent, dropped: disk.Dropped, done: make(chan struct{}), announced: true}
		if disk.Dropped || disk.Tree != nil {
			s.finished = true
			close(s.done)
		} else {
			r.hashes.Add(1)
		}
		r.lineage[disk.ID] = s
		if !disk.Dropped {
			r.snapshots[disk.ID] = s
			r.retainLocked(b, s.stack)
		}
		if n, ok := snapshotNumber(disk.ID); ok && n > r.nextSnapshot {
			r.nextSnapshot = n
		}
	}
	if err = r.removeOrphansLocked(); err != nil {
		cleanup()
		return nil, coded(err)
	}
	if err = r.persistLocked(); err != nil {
		cleanup()
		return nil, coded(err)
	}
	r.hasher = newHasher()
	r.workers.Add(2)
	go r.collectGarbage()
	go r.processNotices()
	// Without folder events, live paths are simply never delegated (see Tracked).
	if r.watcher, err = watch.New(r.folderChanged); err == nil {
		r.watchWake = make(chan struct{}, 1)
		r.workers.Add(1)
		go r.watchFolders()
		r.wakeWatch()
	} else if !errors.Is(err, errors.ErrUnsupported) {
		r.enqueueNotice(baseNotice{fault: "watch source folders: " + err.Error()})
	}
	r.mu.Lock()
	r.scheduleHashesLocked()
	r.mu.Unlock()
	r.wakeGC()
	r.wakeNotices()
	opened = true
	return r, nil
}

// loadBase pairs an opened base with the rules its checkouts were created with.
func loadBase(view *base.Base, name string) (*baseRecord, error) {
	var lock manifest.Lock
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	set, err := lock.Set()
	if err != nil {
		return nil, err
	}
	return &baseRecord{view: view, lock: lock, rules: set}, nil
}

// validateDisk prevents a damaged manifest from escaping the state directory.
func validateDisk(d diskState) error {
	seen := make(map[string]bool)
	for _, b := range d.Bases {
		if !objectPattern.MatchString(b.ID) || !strings.HasPrefix(b.ID, "b_") || seen[b.ID] {
			return fmt.Errorf("invalid or duplicate base id %q", b.ID)
		}
		seen[b.ID] = true
	}
	for _, l := range d.Layers {
		if !objectPattern.MatchString(l.ID) || !strings.HasPrefix(l.ID, "l_") || seen[l.ID] {
			return fmt.Errorf("invalid or duplicate layer id %q", l.ID)
		}
		seen[l.ID] = true
	}
	for _, h := range d.Checkouts {
		if !namePattern.MatchString(h.Name) || seen["h:"+h.Name] {
			return fmt.Errorf("invalid or duplicate checkout %q", h.Name)
		}
		seen["h:"+h.Name] = true
	}
	for _, s := range d.Snapshots {
		if !validSnapshotID(s.ID) || seen[s.ID] {
			return fmt.Errorf("invalid or duplicate snapshot %q", s.ID)
		}
		seen[s.ID] = true
		if s.Layer != "" && (!objectPattern.MatchString(s.Layer) || !strings.HasPrefix(s.Layer, "l_")) {
			return fmt.Errorf("invalid layer in snapshot %s", s.ID)
		}
		if s.StackParent != "" && !validSnapshotID(s.StackParent) {
			return fmt.Errorf("invalid stack parent in snapshot %s", s.ID)
		}
	}
	for _, i := range d.Intents {
		if !namePattern.MatchString(i.Checkout) || (i.Layer != "" && !objectPattern.MatchString(i.Layer)) || (i.Snapshot != "" && !validSnapshotID(i.Snapshot)) {
			return fmt.Errorf("invalid %s intent", i.Op)
		}
	}
	return nil
}

// snapshotID formats the persisted, monotonically allocated snapshot identifier.
func snapshotID(n uint64) string { return "s" + strconv.FormatUint(n, 10) }

// snapshotNumber parses a snapshot identifier's counter.
func snapshotNumber(id string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimPrefix(id, "s"), 10, 64)
	return n, err == nil && strings.HasPrefix(id, "s")
}

// validSnapshotID checks the snapshot identifier grammar.
func validSnapshotID(id string) bool {
	n, ok := snapshotNumber(id)
	return ok && n != 0 && id == snapshotID(n)
}

// snapshotLess sorts counter identifiers numerically rather than lexically.
func snapshotLess(a, b string) bool {
	na, _ := snapshotNumber(a)
	nb, _ := snapshotNumber(b)
	return na < nb
}

// Close stops new mutations, drains hashing and releases persistent base resources.
func (r *Registry) Close() error {
	r.closeOnce.Do(func() {
		// First, while no registry lock is held: Close waits for an event batch
		// that may be reading the registry.
		if r.watcher != nil {
			r.watcher.Close()
		}
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.operations.Wait()
		r.hashes.Wait()
		r.hasher.close()
		close(r.stop)
		r.workers.Wait()
		r.mirror.close()
		r.mu.Lock()
		for _, h := range r.checkouts {
			h.freeze.Lock()
		}
		// A planned stop keeps memory files: they reach the uppers before exit.
		for _, h := range r.checkouts {
			if err := h.fs.Flush(); r.closeErr == nil {
				r.closeErr = err
			}
		}
		if err := r.persistLocked(); r.closeErr == nil {
			r.closeErr = err
		}
		for _, b := range r.bases {
			if err := b.view.Close(); r.closeErr == nil {
				r.closeErr = err
			}
		}
		for _, h := range r.checkouts {
			h.retired.Store(true)
			h.freeze.Unlock()
		}
		r.mu.Unlock()
		if err := r.stateLock.Close(); r.closeErr == nil {
			r.closeErr = err
		}
	})
	return coded(r.closeErr)
}

// Enter pins a checkout generation under the shared freeze lock for one file call.
func (r *Registry) Enter(name string) (*Session, error) {
	r.mu.RLock()
	h := r.checkouts[name]
	closed := r.closed
	r.mu.RUnlock()
	if h == nil || closed {
		return nil, failure("not_found", "no checkout %s", name)
	}
	h.freeze.RLock()
	if h.retired.Load() {
		h.freeze.RUnlock()
		return nil, failure("not_found", "no checkout %s", name)
	}
	return &Session{FS: h.fs, Gen: h.generation.Load(), record: h}, nil
}

// Gen returns a checkout generation without waiting for its freeze lock.
func (r *Registry) Gen(name string) (uint64, bool) {
	r.mu.RLock()
	h := r.checkouts[name]
	r.mu.RUnlock()
	if h == nil || h.retired.Load() {
		return 0, false
	}
	return h.generation.Load(), true
}

// Names returns the sorted live export-root children.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.checkouts))
	for name, h := range r.checkouts {
		if !h.retired.Load() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// RootTime is the virtual export-root directory modification time.
func (r *Registry) RootTime() time.Time { r.mu.RLock(); defer r.mu.RUnlock(); return r.rootTime }

// moveRootLocked guarantees a strictly advancing directory modification time.
func (r *Registry) moveRootLocked() {
	now := time.Now().UTC()
	if !now.After(r.rootTime) {
		now = r.rootTime.Add(time.Nanosecond)
	}
	r.rootTime = now
}

// Ls lists live checkouts and retained snapshots in name and id order.
func (r *Registry) Ls() api.LsReply {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reply := api.LsReply{Checkouts: []api.Checkout{}, Snapshots: []api.Snapshot{}}
	for _, h := range r.checkouts {
		if !h.retired.Load() {
			reply.Checkouts = append(reply.Checkouts, r.checkoutLocked(h))
		}
	}
	for _, s := range r.snapshots {
		reply.Snapshots = append(reply.Snapshots, snapshotOf(s.meta))
	}
	sort.Slice(reply.Checkouts, func(i, j int) bool { return reply.Checkouts[i].Name < reply.Checkouts[j].Name })
	sort.Slice(reply.Snapshots, func(i, j int) bool { return snapshotLess(reply.Snapshots[i].ID, reply.Snapshots[j].ID) })
	return reply
}

// Counts fills a status reply with retained object counts, including objects
// awaiting background GC, and how much the memory tiers hold.
func (r *Registry) Counts(s *api.Status) {
	if r.config.Memory != nil {
		s.Memory.Used, s.Memory.Limit = r.config.Memory.Usage()
	}
	s.BlobCache.Used, s.BlobCache.Limit = r.store.CacheUsage()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.checkouts {
		if !h.retired.Load() {
			s.Checkouts++
		}
	}
	s.Snapshots, s.Bases, s.Layers = len(r.snapshots), len(r.bases), len(r.layers)
}

// checkoutLocked describes a checkout for replies.
func (r *Registry) checkoutLocked(h *checkoutRecord) api.Checkout {
	empty, err := upperEmpty(h)
	meta := h.base.view.Meta()
	src := meta.Dir
	if meta.Remote != nil {
		src = meta.Remote.Source
	}
	return api.Checkout{Name: h.meta.Name, Path: filepath.Join(r.config.Mount, h.meta.Name), Source: src, Base: h.meta.Base, Parent: deref(h.meta.Parent), Git: h.meta.Git, Dirty: err == nil && !empty, Created: h.meta.Created}
}

// snapshotOf describes a snapshot for replies.
func snapshotOf(m snapMeta) api.Snapshot {
	return api.Snapshot{ID: m.ID, Checkout: m.Checkout, Parent: deref(m.Parent), Label: m.Label, Created: m.Created, Tree: deref(m.Tree)}
}

// Listen returns the durable transport binding, or a zero port before first listen.
func (r *Registry) Listen() (string, int) { r.mu.RLock(); defer r.mu.RUnlock(); return r.proto, r.port }

// SetListen durably records the server port for mount reconnection after restart.
func (r *Registry) SetListen(proto string, port int) error {
	if proto == "" || port < 0 || port > 65535 {
		return failure("invalid", "invalid listen transport or port")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return failure("io", "registry is closed")
	}
	oldProto, oldPort := r.proto, r.port
	r.proto, r.port = proto, port
	if err := r.persistLocked(); err != nil {
		r.proto, r.port = oldProto, oldPort
		return coded(err)
	}
	return nil
}

// cloneCheckoutMeta protects registry-owned optional fields from callers.
func cloneCheckoutMeta(h checkoutMeta) checkoutMeta {
	h.Parent = copyString(h.Parent)
	h.Head = copyString(h.Head)
	h.MergeBases = maps.Clone(h.MergeBases)
	return h
}

// cloneSnapMeta protects registry-owned optional fields from callers.
func cloneSnapMeta(s snapMeta) snapMeta {
	s.Parent = copyString(s.Parent)
	s.Tree = copyString(s.Tree)
	return s
}

// randomID allocates a cryptographically random 48-bit on-disk identifier.
func randomID(prefix string) (string, error) {
	var data [6]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(data[:]), nil
}

// upperPath, gitdirPath and snapshotDir name a checkout's or snapshot's private storage.
func (r *Registry) upperPath(name string) string {
	return filepath.Join(r.config.State, "uppers", name)
}
func (r *Registry) gitdirPath(name string) string { return filepath.Join(r.config.State, "git", name) }
func (r *Registry) snapshotDir(id string) string {
	return filepath.Join(r.config.State, "snapshots", id)
}

// objectsPath is the git object directory shared by every checkout on one base.
func (r *Registry) objectsPath(baseID string) string {
	return filepath.Join(r.config.State, "bases", baseID, "objects")
}

// layerStackLocked resolves top-to-bottom identifiers and verifies base consistency.
func (r *Registry) layerStackLocked(ids []string, baseID string) ([]*overlay.Layer, error) {
	layers := make([]*overlay.Layer, 0, len(ids))
	for _, id := range ids {
		l := r.layers[id]
		if l == nil || (l.base != baseID && !l.portable) {
			return nil, fmt.Errorf("missing or incompatible layer %s", id)
		}
		layers = append(layers, l.layer)
	}
	return layers, nil
}

// retainLocked acquires a full-stack reference for a checkout, snapshot or hash.
func (r *Registry) retainLocked(b *baseRecord, stack []string) {
	b.refs++
	for _, id := range stack {
		r.layers[id].refs++
	}
}

// releaseLocked schedules no-longer-referenced objects for asynchronous removal.
func (r *Registry) releaseLocked(b *baseRecord, stack []string) {
	b.refs--
	for _, id := range stack {
		r.layers[id].refs--
	}
	r.wakeGC()
}

// wakeGC coalesces garbage collection requests without blocking file operations.
func (r *Registry) wakeGC() {
	select {
	case r.gcWake <- struct{}{}:
	default:
	}
}

// collectGarbage removes unreferenced layers and bases, then the store objects
// no remaining base or snapshot tree reaches.
func (r *Registry) collectGarbage() {
	defer r.workers.Done()
	for {
		select {
		case <-r.stop:
			return
		case <-r.gcWake:
		}
		r.mu.Lock()
		var deadLayers []*layerRecord
		deadBases := make(map[string]*baseRecord)
		for id, l := range r.layers {
			if l.refs == 0 {
				deadLayers = append(deadLayers, l)
				delete(r.layers, id)
			}
		}
		// A zero-reference base cannot be reacquired: snapshot creates already retain
		// it, and folder creates always capture a new base.
		for id, b := range r.bases {
			if b.refs == 0 && !r.baseReservedLocked(id) {
				deadBases[id] = b
				delete(r.bases, id)
			}
		}
		if len(deadBases) != 0 {
			r.wakeWatch()
		}
		if len(deadLayers) == 0 && len(deadBases) == 0 {
			r.mu.Unlock()
			continue
		}
		if err := r.persistLocked(); err != nil {
			for _, l := range deadLayers {
				r.layers[l.id] = l
			}
			maps.Copy(r.bases, deadBases)
			r.mu.Unlock()
			r.enqueueNotice(baseNotice{fault: "garbage collection: " + err.Error()})
			continue
		}
		roots := r.treeRootsLocked()
		r.mu.Unlock()
		for _, l := range deadLayers {
			if err := os.RemoveAll(l.layer.Dir()); err != nil {
				r.enqueueNotice(baseNotice{fault: "remove layer " + l.id + ": " + err.Error()})
			}
		}
		for id, b := range deadBases {
			if err := b.view.Remove(); err != nil {
				r.enqueueNotice(baseNotice{fault: "remove base " + id + ": " + err.Error()})
			}
		}
		if _, err := r.store.Sweep(roots, time.Now().Add(-gcGrace)); err != nil {
			r.enqueueNotice(baseNotice{fault: "sweep store: " + err.Error()})
		}
	}
}

// treeRootsLocked lists every tree the store must keep: each base capture and each hashed snapshot.
func (r *Registry) treeRootsLocked() []store.OID {
	roots := make([]store.OID, 0, len(r.bases)+len(r.lineage))
	for _, b := range r.bases {
		roots = append(roots, b.view.Meta().Tree)
	}
	for _, s := range r.lineage {
		if s.meta.Tree != nil {
			roots = append(roots, store.OID(*s.meta.Tree))
		}
	}
	for _, h := range r.checkouts {
		for _, tree := range h.meta.MergeBases {
			roots = append(roots, store.OID(tree))
		}
	}
	return roots
}

// baseReservedLocked protects bases belonging to an unfinished create.
func (r *Registry) baseReservedLocked(id string) bool {
	for _, intent := range r.intents {
		if intent.Layer == id {
			return true
		}
	}
	return false
}

// baseConfig supplies storage and nonblocking event callbacks to a base.
func (r *Registry) baseConfig(id string) base.Config {
	return base.Config{ID: id, StateDir: filepath.Join(r.config.State, "bases", id), Store: r.store, OnFault: func(message string) { r.enqueueNotice(baseNotice{base: id, fault: message}) }}
}

// enqueueNotice avoids taking registry locks inside base exposure locks.
func (r *Registry) enqueueNotice(n baseNotice) {
	r.noticeMu.Lock()
	r.notices = append(r.notices, n)
	r.noticeMu.Unlock()
	r.wakeNotices()
}

// wakeNotices coalesces deferred base callback processing.
func (r *Registry) wakeNotices() {
	select {
	case r.noticeWake <- struct{}{}:
	default:
	}
}

// processNotices persists base metadata before reporting faults.
func (r *Registry) processNotices() {
	defer r.workers.Done()
	for {
		select {
		case <-r.stop:
			return
		case <-r.noticeWake:
		}
		r.noticeMu.Lock()
		notices := r.notices
		r.notices = nil
		r.noticeMu.Unlock()
		var deferred []baseNotice
		for _, n := range notices {
			r.mu.Lock()
			if n.base != "" && r.bases[n.base] == nil {
				if r.baseReservedLocked(n.base) {
					deferred = append(deferred, n)
				}
				r.mu.Unlock()
				continue
			}
			err := r.persistLocked()
			r.mu.Unlock()
			if err != nil {
				deferred = append(deferred, n)
				continue
			}
			r.emit(Event{Name: "fault", Payload: map[string]any{"message": n.fault}})
		}
		if len(deferred) != 0 {
			r.noticeMu.Lock()
			r.notices = append(deferred, r.notices...)
			r.noticeMu.Unlock()
		}
	}
}

// emit delivers a durable event without holding registry or freeze locks.
func (r *Registry) emit(event Event) {
	if r.config.Emit != nil {
		r.config.Emit(event)
	}
}

// removeOrphansLocked reclaims filesystem leftovers not named by the recovered manifest.
func (r *Registry) removeOrphansLocked() error {
	for _, kind := range []string{"bases", "layers", "snapshots", "uppers", "git"} {
		entries, err := os.ReadDir(filepath.Join(r.config.State, kind))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			retained := false
			switch kind {
			case "bases":
				retained = r.bases[name] != nil
			case "layers":
				retained = r.layers[name] != nil
			case "snapshots":
				retained = r.lineage[name] != nil
			case "uppers", "git":
				retained = r.checkouts[name] != nil
			}
			if !retained {
				if err = os.RemoveAll(filepath.Join(r.config.State, kind, name)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
