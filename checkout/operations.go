package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/durable"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// gitSource says how a new checkout gets its private git directory, if at all.
type gitSource struct {
	repo  *git.Repo     // build one from this repository's state at capture
	index string        // the captured index copy
	from  string        // copy this existing private git directory instead
	rev   *git.Revision // optional initial revision, already resolved
}

// New opens a checkout from any folder (captured and classified now) or from a snapshot.
func (r *Registry) New(ctx context.Context, args api.NewArgs) (api.NewReply, error) {
	if (args.Dir == "") == (args.Snapshot == "") {
		return api.NewReply{}, failure("invalid", "new needs exactly one of a folder or a snapshot")
	}
	if err := checkContext(ctx); err != nil {
		return api.NewReply{}, err
	}
	if args.Snapshot != "" {
		if args.Rev != "" {
			return api.NewReply{}, failure("invalid", "--rev and --from cannot be combined")
		}
		if len(args.Take) != 0 {
			return api.NewReply{}, failure("invalid", "--take and --from cannot be combined")
		}
		if len(args.Rules) != 0 {
			return api.NewReply{}, failure("invalid", "rules are fixed when a folder is captured; a checkout from a snapshot keeps its base's rules")
		}
		r.mu.Lock()
		s := r.snapshots[args.Snapshot]
		if s == nil {
			r.mu.Unlock()
			return api.NewReply{}, failure("not_found", "no snapshot %s", args.Snapshot)
		}
		name, err := r.pickNameLocked(args.Name, s.meta.Checkout)
		if err != nil {
			r.mu.Unlock()
			return api.NewReply{}, err
		}
		h, err := r.createFromSnapshotLocked(ctx, name, s)
		var reply api.NewReply
		if err == nil {
			reply = api.NewReply{Checkout: r.checkoutLocked(h), Warnings: []string{}}
		}
		r.mu.Unlock()
		if err != nil {
			return api.NewReply{}, coded(err)
		}
		r.created(reply.Checkout)
		return reply, nil
	}
	if !filepath.IsAbs(args.Dir) {
		return api.NewReply{}, failure("invalid", "folder path must be absolute")
	}
	if reply, inMount, err := r.forkMounted(ctx, args.Dir, args); inMount {
		return reply, err
	}
	root, err := source.Resolve(args.Dir)
	if err != nil {
		return api.NewReply{}, coded(err)
	}
	if reply, inMount, err := r.forkMounted(ctx, root, args); inMount {
		return reply, err
	}
	var revision *git.Revision
	if args.Rev != "" {
		repo, repoErr := git.Open(root)
		if repoErr != nil {
			return api.NewReply{}, failure("invalid", "--rev needs a git repository: %s", root)
		}
		revision, err = repo.ResolveRevision(ctx, args.Rev)
		if err != nil {
			return api.NewReply{}, failure("invalid", "cannot resolve revision %q: %v", args.Rev, err)
		}
	}
	takes, err := resolveTakes(ctx, root, args.Take)
	if err != nil {
		return api.NewReply{}, err
	}
	m, err := manifest.Load(root, r.config.UserHome, args.Rules)
	if err != nil {
		return api.NewReply{}, failure("invalid", "%v", err)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return api.NewReply{}, failure("io", "registry is closed")
	}
	name, err := r.pickNameLocked(args.Name, filepath.Base(root))
	if err != nil {
		r.mu.Unlock()
		return api.NewReply{}, err
	}
	id := ""
	for id == "" || r.bases[id] != nil || exists(filepath.Join(r.config.State, "bases", id)) {
		if id, err = randomID("b_"); err != nil {
			r.mu.Unlock()
			return api.NewReply{}, coded(err)
		}
	}
	r.operations.Add(1)
	defer r.operations.Done()
	r.reserved[name] = true
	r.intents[name] = diskIntent{Op: "creating", Checkout: name, Layer: id}
	if err = r.persistIntentLocked(); err != nil {
		delete(r.reserved, name)
		delete(r.intents, name)
		r.mu.Unlock()
		return api.NewReply{}, coded(err)
	}
	r.mu.Unlock()
	config := r.baseConfig(id)
	view, warnings, err := base.CreateRevision(ctx, config, source.Options{Root: root, Rules: m.Rules, Cache: r.statCachePath(root)}, revision, takes...)
	if err != nil {
		if errors.Is(err, base.ErrTake) {
			err = failure("invalid", "%v", err)
		}
		return api.NewReply{}, r.abortCreate(name, config.StateDir, err)
	}
	b := &baseRecord{view: view, lock: m.Lock(), rules: m.Rules}
	gs := gitSource{rev: revision}
	if repo := view.Domains().Repo(""); repo != nil {
		gs.repo = repo
		for _, rr := range view.Capture().Repos {
			if rr.Path == "" {
				gs.index = rr.Index
			}
		}
		err = repo.PrepareObjects(r.objectsPath(id))
	}
	if err == nil {
		err = writeLock(filepath.Join(config.StateDir, lockFile), b.lock)
	}
	if err != nil {
		view.Close()
		return api.NewReply{}, r.abortCreate(name, config.StateDir, err)
	}
	reply, err := r.publishBase(ctx, name, id, b, gs)
	if err != nil {
		return api.NewReply{}, err
	}
	stats := view.Capture().Stats
	reply.Capture, reply.Warnings = &stats, append([]string{}, warnings...)
	return reply, nil
}

// publishBase registers a freshly created base and opens the reserved checkout on it.
func (r *Registry) publishBase(ctx context.Context, name, id string, b *baseRecord, gs gitSource) (api.NewReply, error) {
	r.mu.Lock()
	r.bases[id] = b
	r.wakeWatch()
	err := r.persistIntentLocked()
	var reply api.NewReply
	if err == nil {
		var h *checkoutRecord
		if h, err = r.installCheckoutLocked(ctx, name, b, nil, nil, gs); err == nil {
			delete(r.reserved, name)
			delete(r.intents, name)
			r.moveRootLocked()
			if err = r.persistLocked(); err == nil {
				reply = api.NewReply{Checkout: r.checkoutLocked(h), Warnings: []string{}}
			} else {
				delete(r.checkouts, name)
				r.releaseLocked(b, nil)
			}
		}
	}
	if err != nil {
		cleanupErr := errors.Join(os.RemoveAll(r.upperPath(name)), os.RemoveAll(r.gitdirPath(name)))
		delete(r.reserved, name)
		delete(r.intents, name)
		persistErr := r.persistLocked()
		r.wakeGC()
		r.mu.Unlock()
		r.wakeNotices()
		return api.NewReply{}, coded(errors.Join(err, cleanupErr, persistErr))
	}
	r.mu.Unlock()
	r.created(reply.Checkout)
	r.wakeNotices()
	return reply, nil
}

// forkMounted handles New on a folder inside the daemon's own mount, reporting
// inMount false for any other folder. A checkout sourced from another checkout
// would be served by reading through the mount from the mount's own request
// handlers, which can deadlock the whole mount, so a checkout's root forks that
// checkout instead; the mount root and folders below a checkout's root fail.
func (r *Registry) forkMounted(ctx context.Context, dir string, args api.NewArgs) (reply api.NewReply, inMount bool, err error) {
	rel, inMount := r.underMount(dir)
	if !inMount {
		return api.NewReply{}, false, nil
	}
	name, sub, _ := strings.Cut(filepath.ToSlash(rel), "/")
	switch {
	case rel == ".":
		return api.NewReply{}, true, failure("invalid", "%s is the checkout mount; open a folder or fork a checkout", dir)
	case sub != "":
		return api.NewReply{}, true, failure("invalid", "%s is inside checkout %s; `ovnfs fork %s` copies the whole checkout", dir, name, name)
	case args.Rev != "" || len(args.Take) != 0 || len(args.Rules) != 0:
		return api.NewReply{}, true, failure("invalid", "%s is checkout %s, which new forks; --rev, --take and --rule apply only to folders", dir, name)
	}
	f, err := r.Fork(ctx, api.ForkArgs{Checkout: name, Name: args.Name})
	if err != nil {
		return api.NewReply{}, true, err
	}
	return api.NewReply{Checkout: f.Checkout, Warnings: []string{fmt.Sprintf("%s is checkout %s: forked it at %s", dir, name, f.Snapshot.ID)}}, true, nil
}

// underMount returns dir relative to the daemon's mount when dir is the mount or below it.
func (r *Registry) underMount(dir string) (string, bool) {
	for _, m := range r.mounts {
		if rel, err := filepath.Rel(m, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel, true
		}
	}
	return "", false
}

// mountPaths spells the mount as configured and with its parent's symlinks
// resolved (macOS /tmp is /private/tmp), without looking inside the mount.
func mountPaths(mount string) []string {
	paths := []string{mount}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(mount)); err == nil {
		if real := filepath.Join(parent, filepath.Base(mount)); real != mount {
			paths = append(paths, real)
		}
	}
	return paths
}

// abortCreate undoes a folder create that failed before its base was published.
func (r *Registry) abortCreate(name, baseDir string, cause error) error {
	r.mu.Lock()
	delete(r.reserved, name)
	delete(r.intents, name)
	persistErr := r.persistLocked()
	r.mu.Unlock()
	return coded(errors.Join(cause, os.RemoveAll(baseDir), persistErr))
}

// writeLock durably records a base's resolved manifest.
func writeLock(name string, lock manifest.Lock) error {
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = durable.Sync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// statCachePath names a source folder's stat cache, shared by every capture of it.
func (r *Registry) statCachePath(root string) string {
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(r.config.State, "sources", hex.EncodeToString(sum[:8])+".json")
}

// pickNameLocked validates an explicit name, or derives a free one from hint.
func (r *Registry) pickNameLocked(name, hint string) (string, error) {
	taken := func(n string) bool { return r.checkouts[n] != nil || r.reserved[n] }
	if name != "" {
		if !namePattern.MatchString(name) {
			return "", failure("invalid", "invalid checkout name %q: use lowercase letters, digits, '.', '_' and '-'", name)
		}
		if taken(name) {
			return "", failure("exists", "checkout %s already exists", name)
		}
		return name, nil
	}
	stem := sanitizeName(hint)
	for n := 1; ; n++ {
		candidate := stem
		if n > 1 {
			candidate = stem + "-" + strconv.Itoa(n)
		}
		if !taken(candidate) {
			return candidate, nil
		}
	}
}

// sanitizeName maps a folder name onto the checkout-name grammar.
func sanitizeName(hint string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(hint) {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '.', ch == '_', ch == '-':
			b.WriteRune(ch)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.TrimLeft(b.String(), "._-")
	if len(name) > 56 {
		name = name[:56]
	}
	if name == "" {
		return "checkout"
	}
	return name
}

// installCheckoutLocked creates the upper and private git directory before publication.
func (r *Registry) installCheckoutLocked(ctx context.Context, name string, b *baseRecord, stack []string, parent *string, gs gitSource) (*checkoutRecord, error) {
	layers, err := r.layerStackLocked(stack, b.view.Meta().ID)
	if err != nil {
		return nil, err
	}
	upper := r.upperPath(name)
	if err = os.MkdirAll(upper, 0o755); err != nil {
		return nil, err
	}
	hasGit := gs.repo != nil || gs.from != ""
	if hasGit {
		gitdir := r.gitdirPath(name)
		if err = os.RemoveAll(gitdir); err != nil {
			return nil, err
		}
		objects := r.objectsPath(b.view.Meta().ID)
		if gs.repo != nil {
			err = gs.repo.MakeGitdir(ctx, gitdir, objects, gs.index)
		} else {
			err = git.CopyGitdir(gs.from, gitdir, objects)
		}
		if err == nil && gs.rev != nil {
			err = gs.rev.SetHEAD(gitdir)
		}
		if err == nil {
			err = git.MarkCheckout(ctx, gitdir, name)
		}
		if err == nil {
			err = writeGitfile(upper, gitdir)
		}
		if err != nil {
			return nil, err
		}
	}
	fs, err := overlay.New(upper, layers, b.view.Bottom(), r.config.Memory)
	if err != nil {
		return nil, err
	}
	h := &checkoutRecord{meta: checkoutMeta{Name: name, Base: b.view.Meta().ID, Parent: copyString(parent), Git: hasGit, Created: time.Now().UTC()}, base: b, stack: append([]string{}, stack...), upper: upper, fs: fs}
	h.generation.Store(1)
	r.checkouts[name] = h
	r.retainLocked(b, stack)
	return h, nil
}

// writeGitfile points a checkout's root at its private git directory.
func writeGitfile(upper, gitdir string) error { return git.WriteGitfile(upper, gitdir) }

// createFromSnapshotLocked attaches a new checkout to a snapshot's stack and git state.
func (r *Registry) createFromSnapshotLocked(ctx context.Context, name string, s *snapRecord) (*checkoutRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	r.reserved[name] = true
	r.intents[name] = diskIntent{Op: "creating", Checkout: name, Layer: s.base.view.Meta().ID, Snapshot: s.meta.ID}
	if err := r.persistIntentLocked(); err != nil {
		delete(r.reserved, name)
		delete(r.intents, name)
		return nil, err
	}
	gs := gitSource{}
	if saved := filepath.Join(r.snapshotDir(s.meta.ID), "git"); exists(saved) {
		gs.from = saved
	}
	h, err := r.installCheckoutLocked(ctx, name, s.base, s.stack, &s.meta.ID, gs)
	if err == nil {
		delete(r.reserved, name)
		delete(r.intents, name)
		r.moveRootLocked()
		if err = r.persistLocked(); err == nil {
			return h, nil
		}
		delete(r.checkouts, name)
		r.releaseLocked(s.base, s.stack)
	}
	delete(r.reserved, name)
	delete(r.intents, name)
	return nil, errors.Join(err, os.RemoveAll(r.upperPath(name)), os.RemoveAll(r.gitdirPath(name)), r.persistLocked())
}

// created notifies export watchers and subscribers after publication is durable.
func (r *Registry) created(h api.Checkout) {
	r.emit(Event{Name: "created", Payload: map[string]any{"checkout": h}})
	if r.config.Changed != nil {
		r.config.Changed(h.Name, true)
	}
}

// Snap freezes a checkout's changes into a snapshot and optionally waits for its tree id.
func (r *Registry) Snap(ctx context.Context, args api.SnapArgs) (api.SnapReply, error) {
	if err := checkContext(ctx); err != nil {
		return api.SnapReply{}, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return api.SnapReply{}, failure("io", "registry is closed")
	}
	h := r.checkouts[args.Checkout]
	if h == nil || h.retired.Load() {
		r.mu.Unlock()
		return api.SnapReply{}, failure("not_found", "no checkout %s", args.Checkout)
	}
	s, err := r.freezeLocked(ctx, h, args.Label, true)
	var result snapMeta
	if s != nil {
		result = cloneSnapMeta(s.meta)
	}
	r.mu.Unlock()
	if err != nil {
		return api.SnapReply{}, coded(err)
	}
	r.emit(Event{Name: "snapshotted", Payload: map[string]any{"snapshot": snapshotOf(result)}})
	r.mu.Lock()
	s.announced = true
	r.scheduleHashesLocked()
	r.mu.Unlock()
	if !args.Wait {
		return api.SnapReply{Snapshot: snapshotOf(result)}, nil
	}
	return r.waitHashed(ctx, s)
}

// waitHashed blocks until a snapshot's tree id is known.
func (r *Registry) waitHashed(ctx context.Context, s *snapRecord) (api.SnapReply, error) {
	select {
	case <-ctx.Done():
		return api.SnapReply{}, coded(ctx.Err())
	case <-s.done:
	}
	r.mu.RLock()
	result, err := cloneSnapMeta(s.meta), s.hashErr
	r.mu.RUnlock()
	return api.SnapReply{Snapshot: snapshotOf(result)}, coded(err)
}

// Fork freezes and creates under one registry lock so no competing registry mutation interleaves.
func (r *Registry) Fork(ctx context.Context, args api.ForkArgs) (api.ForkReply, error) {
	if err := checkContext(ctx); err != nil {
		return api.ForkReply{}, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return api.ForkReply{}, failure("io", "registry is closed")
	}
	h := r.checkouts[args.Checkout]
	if h == nil || h.retired.Load() {
		r.mu.Unlock()
		return api.ForkReply{}, failure("not_found", "no checkout %s", args.Checkout)
	}
	child, err := r.pickNameLocked(args.Name, args.Checkout)
	if err != nil {
		r.mu.Unlock()
		return api.ForkReply{}, err
	}
	s, err := r.freezeLocked(ctx, h, args.Label, false)
	if err != nil {
		r.mu.Unlock()
		return api.ForkReply{}, coded(err)
	}
	result := snapshotOf(s.meta)
	created, createErr := r.createFromSnapshotLocked(ctx, child, s)
	var childCheckout api.Checkout
	if createErr != nil {
		createErr = errors.Join(createErr, r.persistLocked())
	} else {
		childCheckout = r.checkoutLocked(created)
	}
	r.mu.Unlock()
	r.emit(Event{Name: "snapshotted", Payload: map[string]any{"snapshot": result}})
	if createErr == nil {
		r.created(childCheckout)
	}
	r.mu.Lock()
	s.announced = true
	r.scheduleHashesLocked()
	r.mu.Unlock()
	if createErr != nil {
		return api.ForkReply{}, coded(createErr)
	}
	return api.ForkReply{Snapshot: result, Checkout: childCheckout}, nil
}

// upperEmpty excludes only infrastructure; every real change requires a layer.
func upperEmpty(h *checkoutRecord) (bool, error) {
	if h.fs.Unflushed() {
		return false, nil
	}
	entries, err := os.ReadDir(h.upper)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != ".git" && entry.Name() != ".wh..wh.work" {
			return false, nil
		}
	}
	return true, nil
}

// saveGit copies a checkout's private git directory into its new snapshot while the checkout is frozen.
func (r *Registry) saveGit(h *checkoutRecord, id string) error {
	if !h.meta.Git {
		return nil
	}
	dir := r.snapshotDir(id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return git.CopyGitdir(r.gitdirPath(h.meta.Name), filepath.Join(dir, "git"), r.objectsPath(h.meta.Base))
}

// freezeLocked freezes an upper; fork defers its durable commit into child publication.
func (r *Registry) freezeLocked(ctx context.Context, h *checkoutRecord, label string, commit bool) (*snapRecord, error) {
	h.freeze.Lock()
	defer h.freeze.Unlock()
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	// The upper directory becomes the layer, so it must hold every change.
	if err := h.fs.Flush(); err != nil {
		return nil, err
	}
	empty, err := upperEmpty(h)
	if err != nil {
		return nil, err
	}
	layerID := ""
	if !empty {
		for {
			layerID, err = randomID("l_")
			if err != nil {
				return nil, err
			}
			if r.layers[layerID] == nil && !exists(filepath.Join(r.config.State, "layers", layerID)) {
				break
			}
		}
	}
	r.nextSnapshot++
	id := snapshotID(r.nextSnapshot)
	parent := h.meta.Head
	if parent == nil {
		parent = h.meta.Parent
	}
	stack := append([]string{}, h.stack...)
	if layerID != "" {
		stack = append([]string{layerID}, stack...)
	}
	stackParent := deref(parent)
	s := &snapRecord{meta: snapMeta{ID: id, Checkout: h.meta.Name, Parent: copyString(parent), Label: label, Created: time.Now().UTC()}, base: h.base, stack: stack, layer: layerID, stackParent: stackParent, done: make(chan struct{})}
	r.snapshots[id] = s
	r.lineage[id] = s
	r.intents[h.meta.Name] = diskIntent{Op: "freezing", Checkout: h.meta.Name, Layer: layerID, Snapshot: id}
	if err = r.persistIntentLocked(); err != nil {
		delete(r.snapshots, id)
		delete(r.lineage, id)
		delete(r.intents, h.meta.Name)
		return nil, err
	}
	oldStack, oldFS, oldHead := h.stack, h.fs, copyString(h.meta.Head)
	renamed, refsChanged, closed := false, false, false
	rollback := func(cause error) (*snapRecord, error) {
		var rollbackErr error
		if refsChanged {
			r.releaseLocked(h.base, s.stack)
			r.releaseLocked(h.base, h.stack)
			r.retainLocked(h.base, oldStack)
			h.stack, h.fs, h.meta.Head = oldStack, oldFS, oldHead
		}
		if refsChanged || closed {
			// Open files reopen on their next call, in the restored upper.
			h.generation.Add(1)
		}
		if renamed {
			if e := os.RemoveAll(h.upper); e != nil {
				rollbackErr = errors.Join(rollbackErr, e)
			}
			if e := os.Rename(filepath.Join(r.config.State, "layers", layerID), h.upper); e != nil {
				rollbackErr = errors.Join(rollbackErr, e)
			}
		}
		delete(r.layers, layerID)
		delete(r.snapshots, id)
		delete(r.lineage, id)
		delete(r.intents, h.meta.Name)
		rollbackErr = errors.Join(rollbackErr, os.RemoveAll(r.snapshotDir(id)))
		// Reopen the restored upper to recreate staging and its in-memory index.
		if renamed {
			layers, e := r.layerStackLocked(oldStack, h.meta.Base)
			if e == nil {
				h.fs, e = overlay.New(h.upper, layers, h.base.view.Bottom(), r.config.Memory)
			}
			rollbackErr = errors.Join(rollbackErr, e)
		}
		if rollbackErr == nil {
			rollbackErr = r.persistLocked()
		}
		return nil, errors.Join(cause, rollbackErr)
	}
	if err = r.saveGit(h, id); err != nil {
		return rollback(err)
	}
	if layerID != "" {
		layerDir := filepath.Join(r.config.State, "layers", layerID)
		// Windows cannot rename the upper while a file in it is open; open
		// files move to the next generation's upper on their next call anyway.
		oldFS.CloseFiles()
		closed = true
		if err = os.Rename(h.upper, layerDir); err != nil {
			return rollback(err)
		}
		renamed = true
		if err = os.RemoveAll(filepath.Join(layerDir, ".wh..wh.work")); err != nil {
			return rollback(err)
		}
		layer, indexErr := oldFS.Freeze(layerDir)
		if indexErr != nil {
			return rollback(indexErr)
		}
		r.layers[layerID] = &layerRecord{id: layerID, base: h.meta.Base, layer: layer}
		layers, stackErr := r.layerStackLocked(stack, h.meta.Base)
		if stackErr != nil {
			return rollback(stackErr)
		}
		if err = os.MkdirAll(h.upper, 0o755); err != nil {
			return rollback(err)
		}
		if h.meta.Git {
			if err = writeGitfile(h.upper, r.gitdirPath(h.meta.Name)); err != nil {
				return rollback(err)
			}
		}
		fresh, freshErr := overlay.New(h.upper, layers, h.base.view.Bottom(), r.config.Memory)
		if freshErr != nil {
			return rollback(freshErr)
		}
		h.fs = fresh
	}
	r.releaseLocked(h.base, oldStack)
	h.stack = stack
	r.retainLocked(h.base, stack)
	r.retainLocked(s.base, s.stack)
	refsChanged = true
	h.meta.Head = copyString(&id)
	h.generation.Add(1)
	delete(r.intents, h.meta.Name)
	if commit {
		if err = r.persistLocked(); err != nil {
			return rollback(err)
		}
	}
	r.hashes.Add(1)
	return s, nil
}

// Rm retires handles while frozen and deletes writable state; snapshots stay until dropped.
func (r *Registry) Rm(ctx context.Context, args api.RmArgs) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	name := args.Checkout
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return failure("io", "registry is closed")
	}
	h := r.checkouts[name]
	if h == nil {
		r.mu.Unlock()
		return failure("not_found", "no checkout %s", name)
	}
	h.freeze.Lock()
	r.intents[name] = diskIntent{Op: "removing", Checkout: name, Layer: h.meta.Base}
	if err := r.persistIntentLocked(); err != nil {
		delete(r.intents, name)
		h.freeze.Unlock()
		r.mu.Unlock()
		return coded(err)
	}
	h.retired.Store(true)
	if r.config.Retire != nil {
		r.config.Retire(name)
	}
	h.fs.Discard()
	h.fs.CloseFiles()
	if err := errors.Join(os.RemoveAll(r.gitdirPath(name)), os.RemoveAll(h.upper)); err != nil {
		h.freeze.Unlock()
		r.mu.Unlock()
		return coded(err)
	}
	delete(r.checkouts, name)
	delete(r.intents, name)
	r.releaseLocked(h.base, h.stack)
	r.moveRootLocked()
	err := r.persistLocked()
	if err != nil {
		r.intents[name] = diskIntent{Op: "removing", Checkout: name, Layer: h.meta.Base}
	}
	h.freeze.Unlock()
	r.mu.Unlock()
	if err != nil {
		return coded(err)
	}
	r.emit(Event{Name: "removed", Payload: map[string]any{"name": name}})
	if r.config.Changed != nil {
		r.config.Changed(name, false)
	}
	return nil
}

// Drop releases a snapshot and its retained stack.
func (r *Registry) Drop(ctx context.Context, args api.DropArgs) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	id := args.Snapshot
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return failure("io", "registry is closed")
	}
	s := r.snapshots[id]
	if s == nil {
		return failure("not_found", "no snapshot %s", id)
	}
	delete(r.snapshots, id)
	s.dropped = true
	if err := r.persistLocked(); err != nil {
		s.dropped = false
		r.snapshots[id] = s
		r.lineage[id] = s
		return coded(err)
	}
	r.releaseLocked(s.base, s.stack)
	if !s.hashing && !s.finished {
		s.finished = true
		s.hashErr = failure("not_found", "snapshot %s was dropped", id)
		close(s.done)
		r.hashes.Done()
	}
	if err := os.RemoveAll(filepath.Join(r.snapshotDir(id), "git")); err != nil {
		return coded(err)
	}
	r.scheduleHashesLocked()
	return nil
}

// ancestorTreeLocked finds the nearest hashed ancestor of a stack (following
// snapshot parents from start) whose layers are a suffix of the stack, and the
// newer layers on top of it, oldest first. ready is false while an ancestor hashes.
func (r *Registry) ancestorTreeLocked(b *baseRecord, stack []string, start *string) (parent store.OID, layers []*overlay.Layer, ready bool, err error) {
	parent = b.view.Meta().Tree
	parentStack := []string{}
	visited := make(map[string]bool)
	for ancestor := start; ancestor != nil; {
		if visited[*ancestor] {
			return "", nil, false, fmt.Errorf("snapshot parent cycle at %s", *ancestor)
		}
		visited[*ancestor] = true
		s := r.lineage[*ancestor]
		if s == nil {
			break
		}
		if !s.finished {
			return "", nil, false, nil
		}
		if s.hashErr == nil && s.meta.Tree != nil {
			parent, parentStack = store.OID(*s.meta.Tree), s.stack
			break
		}
		ancestor = s.meta.Parent
	}
	prefix := len(stack) - len(parentStack)
	if prefix < 0 || !sameStack(stack[prefix:], parentStack) {
		parent, prefix = b.view.Meta().Tree, len(stack)
	}
	for i := prefix - 1; i >= 0; i-- {
		l := r.layers[stack[i]]
		if l == nil {
			return "", nil, false, fmt.Errorf("missing layer %s", stack[i])
		}
		layers = append(layers, l.layer)
	}
	return parent, layers, true, nil
}

// scheduleHashesLocked starts only jobs whose nearest retained ancestor has resolved.
func (r *Registry) scheduleHashesLocked() {
	ids := make([]string, 0, len(r.snapshots))
	for id := range r.snapshots {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return snapshotLess(ids[i], ids[j]) })
	for _, id := range ids {
		s := r.snapshots[id]
		if s.hashing || s.finished || !s.announced {
			continue
		}
		parent, layers, ready, err := r.ancestorTreeLocked(s.base, s.stack, s.meta.Parent)
		if err != nil {
			r.finishHashLocked(s, "", err)
			continue
		}
		if !ready {
			continue
		}
		meta := s.base.view.Meta()
		stack, err := r.layerStackLocked(s.stack, meta.ID)
		if err != nil {
			r.finishHashLocked(s, "", err)
			continue
		}
		view, err := overlay.New("", stack, s.base.view.Bottom(), nil)
		if err != nil {
			r.finishHashLocked(s, "", err)
			continue
		}
		s.hashing = true
		r.retainLocked(s.base, s.stack)
		job := hashJob{Lineage: meta.ID, Snapshot: id, Base: s.base, Parent: parent, Layers: layers, View: view}
		job.Done = func(tree store.OID, hashErr error) { r.hashDone(s, tree, hashErr) }
		r.hasher.queue(job)
	}
}

// sameStack compares top-to-bottom immutable layer identities.
func sameStack(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// finishHashLocked routes preparation failures through the durable job completion path.
func (r *Registry) finishHashLocked(s *snapRecord, tree store.OID, err error) {
	s.hashing = true
	r.retainLocked(s.base, s.stack)
	go r.hashDone(s, tree, err)
}

// hashDone publishes completed tree ids and releases active-job references.
func (r *Registry) hashDone(s *snapRecord, tree store.OID, hashErr error) {
	r.mu.Lock()
	if hashErr == nil && !s.dropped {
		id := string(tree)
		s.meta.Tree = &id
	}
	s.hashErr = hashErr
	persistErr := r.persistLocked()
	if persistErr != nil {
		s.meta.Tree = nil
		s.hashErr = errors.Join(hashErr, persistErr)
	}
	result := snapshotOf(s.meta)
	reportErr := s.hashErr
	dropped := s.dropped
	r.mu.Unlock()
	if !dropped && persistErr == nil {
		if reportErr == nil {
			r.emit(Event{Name: "hashed", Payload: map[string]any{"snapshot": result}})
		} else {
			r.emit(Event{Name: "hash_failed", Payload: map[string]any{"snapshot": result, "message": reportErr.Error()}})
		}
	}
	r.mu.Lock()
	s.finished = true
	close(s.done)
	r.releaseLocked(s.base, s.stack)
	r.scheduleHashesLocked()
	r.mu.Unlock()
	r.hashes.Done()
}
