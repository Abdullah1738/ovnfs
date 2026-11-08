package fetch

import (
	"context"
	"fmt"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// unpackLimit is git's default fetch.unpackLimit: transfers of fewer objects
// are stored loose, larger ones as a pack.
const unpackLimit = 100

// LocalOptions tunes Local.
type LocalOptions struct {
	// Depth limits history to that many commits from each want (1: the
	// wanted commits and their trees only); 0 is unlimited.
	Depth int
	// SourceShallow is the source repository's shallow boundary: those
	// commits are treated as roots and become shallow in the target too.
	SourceShallow []object.ID
	// TargetShallow is the target's shallow boundary. A depth-limited fetch
	// that reaches past one of these commits deepens it (Result.Unshallow).
	TargetShallow []object.ID
	// PackDir is the target's objects/pack directory, where a large transfer
	// is written as one pack.
	PackDir string
	// MaxObjectSize, when > 0, refuses source objects larger than this many
	// bytes before reading them.
	MaxObjectSize int64
}

// Result describes what a fetch stored.
type Result struct {
	// Shallow lists commits the target must add to its shallow file: their
	// parents were not transferred.
	Shallow []object.ID
	// Unshallow lists commits to remove from the shallow file because their
	// parents now are present.
	Unshallow []object.ID
	// Pack is the name of the pack written, "" if objects were stored loose
	// or nothing was missing.
	Pack string
}

// Local copies into dst every object reachable from wants that dst lacks,
// without a subprocess, as `git fetch` from a local repository effectively
// does. The walk stops at commits and trees dst already has (they are taken
// to be complete) and at the source's shallow commits, and honours
// opts.Depth. Fewer than 100 objects are written loose, more as one pack.
func Local(ctx context.Context, src, dst *object.DB, wants []object.ID, opts LocalOptions) (Result, error) {
	if src.Format() != dst.Format() {
		return Result{}, fmt.Errorf("fetch: source objects are %s, target objects are %s", src.Format(), dst.Format())
	}
	// One rescan up front lets the walk's many existence checks on dst
	// skip rescanning on every miss.
	if err := dst.Rescan(); err != nil {
		return Result{}, err
	}
	w := &localWalk{
		ctx:     ctx,
		src:     src,
		dst:     dst,
		opts:    opts,
		seen:    make(map[object.ID]struct{}),
		srcShal: idSet(opts.SourceShallow),
		dstShal: idSet(opts.TargetShallow),
		out:     &objectSink{dst: dst, packDir: opts.PackDir},
	}
	res, err := w.run(wants)
	if err != nil {
		w.out.abort()
		return Result{}, err
	}
	res.Pack, err = w.out.finish()
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

func idSet(ids []object.ID) map[object.ID]struct{} {
	m := make(map[object.ID]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

type localWalk struct {
	ctx              context.Context
	src, dst         *object.DB
	opts             LocalOptions
	seen             map[object.ID]struct{} // objects already visited
	srcShal, dstShal map[object.ID]struct{}
	out              *objectSink
}

func (w *localWalk) visit(id object.ID) bool {
	if _, ok := w.seen[id]; ok {
		return false
	}
	w.seen[id] = struct{}{}
	return true
}

func (w *localWalk) run(wants []object.ID) (Result, error) {
	var commits []object.ID
	for _, want := range wants {
		id, kind, err := w.peel(want)
		if err != nil {
			return Result{}, err
		}
		switch kind {
		case object.Commit:
			commits = append(commits, id)
		case object.Tree:
			err = w.tree(id)
		case object.Blob:
			err = w.blob(id)
		}
		if err != nil {
			return Result{}, err
		}
	}
	return w.history(commits)
}

// peel follows a want through annotated tags, sending each tag dst lacks,
// and returns the object reached. Kind 0 means the chain was already walked.
func (w *localWalk) peel(id object.ID) (object.ID, object.Kind, error) {
	for {
		if err := w.ctx.Err(); err != nil {
			return id, 0, err
		}
		kind, _, err := w.src.Header(id)
		if err != nil {
			return id, 0, fmt.Errorf("fetch: reading %s from source: %w", id, err)
		}
		if kind != object.Tag {
			return id, kind, nil
		}
		if !w.visit(id) {
			return id, 0, nil
		}
		_, data, err := w.read(id)
		if err != nil {
			return id, 0, err
		}
		if !w.dst.Contains(id) {
			if err := w.out.add(kind, data); err != nil {
				return id, 0, err
			}
		}
		tag, err := object.ParseTag(w.src.Format(), data)
		if err != nil {
			return id, 0, fmt.Errorf("fetch: tag %s: %w", id, err)
		}
		id = tag.Object
	}
}

// history walks commits breadth-first so each is reached at its smallest
// depth, which decides where a depth-limited history is cut.
func (w *localWalk) history(wants []object.ID) (Result, error) {
	var res Result
	type item struct {
		id    object.ID
		depth int
	}
	var queue []item
	for _, id := range wants {
		if w.visit(id) {
			queue = append(queue, item{id, 1})
		}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if err := w.ctx.Err(); err != nil {
			return res, err
		}
		_, deepen := w.dstShal[it.id]
		deepen = deepen && w.opts.Depth > 0 && it.depth < w.opts.Depth
		have := w.dst.Contains(it.id)
		if have && !deepen {
			continue // complete in the target
		}
		kind, data, err := w.read(it.id)
		if err != nil {
			return res, err
		}
		if kind != object.Commit {
			return res, fmt.Errorf("fetch: %s is a %s, not a commit", it.id, kind)
		}
		c, err := object.ParseCommit(w.src.Format(), data)
		if err != nil {
			return res, fmt.Errorf("fetch: commit %s: %w", it.id, err)
		}
		if !have {
			if err := w.out.add(kind, data); err != nil {
				return res, err
			}
			if err := w.tree(c.Tree); err != nil {
				return res, err
			}
		}
		if _, ok := w.srcShal[it.id]; ok {
			if !have {
				res.Shallow = append(res.Shallow, it.id)
			}
			continue
		}
		if w.opts.Depth > 0 && it.depth >= w.opts.Depth {
			// git lists the commits at the cut even when they are roots.
			res.Shallow = append(res.Shallow, it.id)
			continue
		}
		if have && len(c.Parents) > 0 {
			res.Unshallow = append(res.Unshallow, it.id)
		}
		for _, p := range c.Parents {
			if w.visit(p) {
				queue = append(queue, item{p, it.depth + 1})
			}
		}
	}
	return res, nil
}

// tree sends a tree dst lacks and, recursively, its missing entries.
func (w *localWalk) tree(id object.ID) error {
	if !w.visit(id) || w.dst.Contains(id) {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	kind, data, err := w.read(id)
	if err != nil {
		return err
	}
	if kind != object.Tree {
		return fmt.Errorf("fetch: %s is a %s, not a tree", id, kind)
	}
	entries, err := object.ParseTree(w.src.Format(), data)
	if err != nil {
		return fmt.Errorf("fetch: tree %s: %w", id, err)
	}
	if err := w.out.add(kind, data); err != nil {
		return err
	}
	for _, e := range entries {
		switch e.Mode.Kind() {
		case object.Tree:
			err = w.tree(e.ID)
		case object.Blob:
			err = w.blob(e.ID)
		}
		// Gitlinks name commits of another repository: not ours to copy.
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *localWalk) blob(id object.ID) error {
	if !w.visit(id) || w.dst.Contains(id) {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	kind, data, err := w.read(id)
	if err != nil {
		return err
	}
	return w.out.add(kind, data)
}

// read reads a source object, refusing one over the size limit before
// allocating for it.
func (w *localWalk) read(id object.ID) (object.Kind, []byte, error) {
	if w.opts.MaxObjectSize > 0 {
		kind, size, err := w.src.Header(id)
		if err != nil {
			return 0, nil, fmt.Errorf("fetch: reading %s from source: %w", id, err)
		}
		if size > w.opts.MaxObjectSize {
			return 0, nil, fmt.Errorf("fetch: source %s %s is %d bytes, over the %d-byte object size limit", kind, id, size, w.opts.MaxObjectSize)
		}
	}
	kind, data, err := w.src.Read(id)
	if err != nil {
		return 0, nil, fmt.Errorf("fetch: reading %s from source: %w", id, err)
	}
	return kind, data, nil
}

// objectSink stores objects loose until there are unpackLimit of them, then
// switches to a pack, so small transfers stay loose like git's and large ones
// avoid thousands of files, without buffering more than a few objects.
type objectSink struct {
	dst     *object.DB
	packDir string
	pending []pendingObject
	pw      *object.PackWriter
}

type pendingObject struct {
	kind object.Kind
	data []byte
}

func (s *objectSink) add(kind object.Kind, data []byte) error {
	if s.pw != nil {
		_, err := s.pw.Add(kind, data)
		return err
	}
	s.pending = append(s.pending, pendingObject{kind, data})
	if len(s.pending) < unpackLimit {
		return nil
	}
	if s.packDir == "" {
		return fmt.Errorf("fetch: no pack directory for a transfer of %d or more objects", unpackLimit)
	}
	pw, err := object.NewPackWriter(s.packDir, s.dst.Format())
	if err != nil {
		return err
	}
	s.pw = pw
	for _, o := range s.pending {
		if _, err := pw.Add(o.kind, o.data); err != nil {
			return err
		}
	}
	s.pending = nil
	return nil
}

// finish writes what was collected and returns the pack name, if any.
func (s *objectSink) finish() (string, error) {
	if s.pw != nil {
		return s.pw.Commit()
	}
	for _, o := range s.pending {
		if _, err := s.dst.WriteLoose(o.kind, o.data); err != nil {
			return "", err
		}
	}
	s.pending = nil
	return "", nil
}

func (s *objectSink) abort() {
	if s.pw != nil {
		s.pw.Abort()
	}
	s.pending = nil
}
