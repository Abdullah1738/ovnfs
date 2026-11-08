package rev

import (
	"container/heap"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// Paint flags of the merge-base walk (technical/paint-down-to-common).
const (
	flagParent1 uint8 = 1 << iota
	flagParent2
	flagStale
	flagResult
	flagSeen // visited by reaches
)

// noPos marks a commit whose commit-graph position is not known (yet).
const noPos = -1

// commitInfo is a commit as the walks see it.
type commitInfo struct {
	id      object.ID
	parents []object.ID
	gen     uint64 // generation number; genInfinity when unknown
	date    int64  // committer time
	msg     string // only when the walker keeps messages
	flags   uint8
	loaded  bool
	pos     int64 // commit-graph position, noPos if unknown or absent
}

// walker loads commits for one operation, from the commit-graph when one
// applies and otherwise by parsing them.
type walker struct {
	src          *Source
	graph        *commitGraph
	nodes        map[object.ID]*commitInfo
	keepMessages bool
}

// newWalker prepares a walk; withGraph opens the commit-graph, which the
// caller must release with close. Like git, the graph is not used when
// replace refs or shallow commits change the history it recorded, or when
// core.commitGraph is false.
func newWalker(src *Source, withGraph bool) *walker {
	w := &walker{src: src, nodes: map[object.ID]*commitInfo{}}
	if !withGraph || src.Replaced || len(src.Shallow) > 0 {
		return w
	}
	if src.Config != nil {
		if on, found, err := src.Config.Bool("core.commitgraph"); err == nil && found && !on {
			return w
		}
	}
	g, err := openCommitGraph(src.Objects.Dirs(), src.Objects.Format())
	if err == nil {
		w.graph = g
	}
	return w
}

func (w *walker) close() {
	if w.graph != nil {
		w.graph.close()
		w.graph = nil
	}
}

func (w *walker) node(id object.ID) *commitInfo {
	n := w.nodes[id]
	if n == nil {
		n = &commitInfo{id: id, gen: genInfinity, pos: noPos}
		w.nodes[id] = n
	}
	return n
}

// load fills in n's parents, generation and date.
func (w *walker) load(n *commitInfo) error {
	if n.loaded {
		return nil
	}
	if w.graph != nil && !w.keepMessages {
		pos, ok := uint32(n.pos), n.pos != noPos
		if !ok {
			pos, ok = w.graph.find(n.id)
		}
		if ok {
			gc, err := w.graph.commit(pos)
			if err == nil {
				parents := make([]object.ID, 0, len(gc.parents))
				for _, p := range gc.parents {
					id, perr := w.graph.id(p)
					if perr != nil {
						err = perr
						break
					}
					parents = append(parents, id)
					// Remember where the parent is to skip its lookup.
					if pn := w.node(id); pn.pos == noPos {
						pn.pos = int64(p)
					}
				}
				if err == nil {
					n.parents, n.gen, n.date, n.loaded = parents, gc.gen, gc.date, true
					return nil
				}
			}
			// A corrupt graph record: fall back to the commit itself.
		}
	}
	f := w.src.Objects.Format()
	kind, data, err := w.src.Objects.Read(n.id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return unknown("commit %s does not exist", n.id)
		}
		return err
	}
	if kind != object.Commit {
		return fmt.Errorf("%w: %s is a %s, not a commit", ErrPeel, n.id, kind)
	}
	c, err := object.ParseCommit(f, data)
	if err != nil {
		return err
	}
	if !w.src.Shallow[n.id] {
		n.parents = c.Parents
	}
	n.date = c.Committer.When.Unix()
	if w.keepMessages {
		n.msg = c.Message
	}
	n.loaded = true
	return nil
}

// parentIDs returns a commit's parents (none for a shallow commit).
func (w *walker) parentIDs(id object.ID) ([]object.ID, error) {
	n := w.node(id)
	if err := w.load(n); err != nil {
		return nil, err
	}
	return n.parents, nil
}

// message returns a commit's message.
func (w *walker) message(id object.ID) (string, error) {
	n := w.node(id)
	if n.loaded && w.keepMessages {
		return n.msg, nil
	}
	_, data, err := w.src.Objects.Read(id)
	if err != nil {
		return "", err
	}
	c, err := object.ParseCommit(w.src.Objects.Format(), data)
	if err != nil {
		return "", err
	}
	return c.Message, nil
}

// queue orders commits by generation, then commit date, newest first;
// ties leave in insertion order, which decides the order merge bases are
// found in.
type queue struct {
	items []queueItem
	seq   uint64
}

type queueItem struct {
	n   *commitInfo
	seq uint64
}

func (q *queue) Len() int { return len(q.items) }
func (q *queue) Less(i, j int) bool {
	a, b := q.items[i], q.items[j]
	if a.n.gen != b.n.gen {
		return a.n.gen > b.n.gen
	}
	if a.n.date != b.n.date {
		return a.n.date > b.n.date
	}
	return a.seq < b.seq
}
func (q *queue) Swap(i, j int) { q.items[i], q.items[j] = q.items[j], q.items[i] }
func (q *queue) Push(x any)    { q.items = append(q.items, x.(queueItem)) }
func (q *queue) Pop() any {
	last := q.items[len(q.items)-1]
	q.items = q.items[:len(q.items)-1]
	return last
}

func (q *queue) push(n *commitInfo) {
	heap.Push(q, queueItem{n: n, seq: q.seq})
	q.seq++
}

func (q *queue) pop() *commitInfo { return heap.Pop(q).(queueItem).n }

// firstByDate walks back from starts in queue order and returns the first
// commit match accepts.
func (w *walker) firstByDate(starts []object.ID, match func(*commitInfo) (bool, error)) (object.ID, bool, error) {
	w.keepMessages = true
	defer func() { w.keepMessages = false }()
	seen := map[object.ID]bool{}
	q := &queue{}
	for _, id := range starts {
		if seen[id] {
			continue
		}
		seen[id] = true
		n := w.node(id)
		if err := w.load(n); err != nil {
			return object.ID{}, false, err
		}
		q.push(n)
	}
	for q.Len() > 0 {
		n := q.pop()
		ok, err := match(n)
		if err != nil {
			return object.ID{}, false, err
		}
		if ok {
			return n.id, true, nil
		}
		for _, p := range n.parents {
			if seen[p] {
				continue
			}
			seen[p] = true
			pn := w.node(p)
			if err := w.load(pn); err != nil {
				return object.ID{}, false, err
			}
			q.push(pn)
		}
	}
	return object.ID{}, false, nil
}

// peelCommit follows tags from id to a commit.
func peelCommit(src *Source, id object.ID) (object.ID, error) {
	r := newResolver(src, Options{})
	return r.peel(id, object.Commit, id.String())
}

// IsAncestor reports whether commit a is reachable from commit b (a commit
// is its own ancestor), as `git merge-base --is-ancestor a b`. Tags are
// peeled. The walk prunes by generation number when a commit-graph exists.
func IsAncestor(src *Source, a, b object.ID) (bool, error) {
	a, err := peelCommit(src, a)
	if err != nil {
		return false, err
	}
	if b, err = peelCommit(src, b); err != nil {
		return false, err
	}
	w := newWalker(src, true)
	defer w.close()
	return w.reaches(b, a)
}

// reaches reports whether target is reachable from from.
func (w *walker) reaches(from, target object.ID) (bool, error) {
	if from == target {
		return true, nil
	}
	tn := w.node(target)
	if err := w.load(tn); err != nil {
		return false, err
	}
	fn := w.node(from)
	if err := w.load(fn); err != nil {
		return false, err
	}
	seen := []*commitInfo{fn}
	fn.flags |= flagSeen
	defer func() {
		for _, n := range seen {
			n.flags &^= flagSeen
		}
	}()
	q := &queue{}
	q.push(fn)
	for q.Len() > 0 {
		n := q.pop()
		for _, p := range n.parents {
			if p == target {
				return true, nil
			}
			pn := w.node(p)
			if pn.flags&flagSeen != 0 {
				continue
			}
			pn.flags |= flagSeen
			seen = append(seen, pn)
			if err := w.load(pn); err != nil {
				return false, err
			}
			// A commit with a lower generation than the target cannot
			// reach it (commit-graph reachability property).
			if tn.gen != genInfinity && pn.gen != genInfinity && pn.gen < tn.gen {
				continue
			}
			q.push(pn)
		}
	}
	return false, nil
}

// MergeBase returns the best common ancestor of commits a and b, the one
// `git merge-base a b` prints, and false when they share no history.
func MergeBase(src *Source, a, b object.ID) (object.ID, bool, error) {
	bases, err := mergeBases(src, a, b, true)
	if err != nil || len(bases) == 0 {
		return object.ID{}, false, err
	}
	return bases[0], true, nil
}

// MergeBases returns every best common ancestor of commits a and b, in the
// order `git merge-base --all a b` prints them.
func MergeBases(src *Source, a, b object.ID) ([]object.ID, error) {
	return mergeBases(src, a, b, false)
}

// mergeBases computes the merge bases; single stops at the first candidate
// once the walk is ordered by generation numbers, which then cannot be
// redundant. That candidate need not be the newest base, so with a
// commit-graph `git merge-base` may print a different base than the first
// of --all, and so does this.
func mergeBases(src *Source, a, b object.ID, single bool) ([]object.ID, error) {
	a, err := peelCommit(src, a)
	if err != nil {
		return nil, err
	}
	if b, err = peelCommit(src, b); err != nil {
		return nil, err
	}
	w := newWalker(src, true)
	defer w.close()
	if a == b {
		return []object.ID{a}, nil
	}
	found, err := w.paintDownToCommon(a, b, single)
	if err != nil {
		return nil, err
	}
	if single && len(found) == 1 && w.ordered(found[0]) {
		return []object.ID{found[0].id}, nil
	}
	// A candidate that a later candidate's STALE paint reached is its
	// ancestor, hence no best common ancestor.
	cands := slices.DeleteFunc(found, func(n *commitInfo) bool { return n.flags&flagStale != 0 })
	slices.SortStableFunc(cands, func(x, y *commitInfo) int {
		switch {
		case x.date > y.date:
			return -1
		case x.date < y.date:
			return 1
		}
		return 0
	})
	if len(cands) <= 1 {
		return ids(cands), nil
	}
	return w.removeRedundant(cands)
}

func ids(ns []*commitInfo) []object.ID {
	out := make([]object.ID, len(ns))
	for i, n := range ns {
		out[i] = n.id
	}
	return out
}

// paintDownToCommon paints commits reachable from one with PARENT1 and from
// two with PARENT2; commits reached by both are merge-base candidates, in
// the order found, and mark their ancestors STALE.
func (w *walker) paintDownToCommon(one, two object.ID, single bool) ([]*commitInfo, error) {
	on, tn := w.node(one), w.node(two)
	for _, n := range []*commitInfo{on, tn} {
		if err := w.load(n); err != nil {
			return nil, err
		}
	}
	q := &queue{}
	on.flags |= flagParent1
	q.push(on)
	tn.flags |= flagParent2
	q.push(tn)
	var result []*commitInfo
	for q.hasNonStale() {
		n := q.pop()
		flags := n.flags & (flagParent1 | flagParent2 | flagStale)
		if flags == flagParent1|flagParent2 {
			if n.flags&flagResult == 0 {
				n.flags |= flagResult
				result = append(result, n)
				if single && w.ordered(n) {
					return result, nil
				}
			}
			flags |= flagStale
		}
		for _, p := range n.parents {
			pn := w.node(p)
			if pn.flags&flags == flags {
				continue
			}
			if err := w.load(pn); err != nil {
				return nil, err
			}
			pn.flags |= flags
			q.push(pn)
		}
	}
	return result, nil
}

// genV1Max is the saturated topological level of v1 commit-graphs.
const genV1Max = 0x3FFFFFFF

// ordered reports whether n lies in the region where generation numbers
// order the walk topologically (paint-down-to-common: not INFINITY, not a
// saturated v1 level).
func (w *walker) ordered(n *commitInfo) bool {
	if w.graph == nil || n.gen == genInfinity {
		return false
	}
	return w.graph.v2 || n.gen < genV1Max
}

func (q *queue) hasNonStale() bool {
	for _, it := range q.items {
		if it.n.flags&flagStale == 0 {
			return true
		}
	}
	return false
}

// removeRedundant drops candidates reachable from another candidate,
// keeping the order of the rest.
func (w *walker) removeRedundant(cands []*commitInfo) ([]object.ID, error) {
	var out []object.ID
	for i, c := range cands {
		redundant := false
		for j, other := range cands {
			if i == j {
				continue
			}
			ok, err := w.reaches(other.id, c.id)
			if err != nil {
				return nil, err
			}
			if ok {
				redundant = true
				break
			}
		}
		if !redundant {
			out = append(out, c.id)
		}
	}
	return out, nil
}
