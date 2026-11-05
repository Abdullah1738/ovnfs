// Package glob finds the files and directories below a root whose
// walk-relative paths match a glob, using the shared filesystem walker.
//
// One compiled [pattern.Glob] both selects entries and prunes the walk
// (depth bound and subtrees no match can reach), one filter stage applies
// the type filter, and results come back in path order or newest-first with
// an honest pre-limit total:
//
//	res, err := glob.Glob(ctx, "/repo", glob.Options{
//		Pattern: "*.go",
//		Limit:   100,
//		Walk:    walk.Options{Gitignore: true, SkipGit: true, SkipNodeModules: true},
//	})
package glob

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/walk"
)

// Options configures a [Glob] search.
type Options struct {
	// Pattern is matched against '/'-separated paths relative to the root
	// (directories without a trailing slash), with [pattern] semantics after
	// omp's normalisation (see [Selector]): backslashes become '/' and
	// unclosed '{' groups are closed.
	Pattern string
	// Anchored matches Pattern against the whole relative path as written.
	// When false (the default recursive mode), a pattern without '/' that
	// neither starts with "**" nor is an exact brace union (`{a,b}`) matches
	// at any depth, as if prefixed "**/".
	Anchored bool
	// Type keeps only entries of this type; 0 keeps every type. With File or
	// Dir, a symlink whose target has that type counts as one and is
	// reported with the target's type (and, under SortByMtime, the target's
	// size and mtime). Without a filter symlinks are reported as Symlink.
	Type fsys.Type
	// Limit caps the returned matches; 0 returns all of them.
	Limit int
	// SortByMtime orders matches newest first (by millisecond, ties by
	// path) and fills Size and Mtime; Limit then keeps the newest.
	// Otherwise matches are in byte order of Path and carry no metadata.
	SortByMtime bool
	// Walk carries the traversal options (filesystem, hidden, ignore files,
	// .git and node_modules pruning, workers, listing cache). Glob overrides
	// what the pattern decides: MaxDepth is tightened to the pattern's depth
	// bound, node_modules is descended whenever Pattern mentions it, and
	// Detail, Sorted, and Filter are set per search.
	Walk walk.Options
}

// Match is one matching entry.
type Match struct {
	// Path is relative to the root, '/'-separated.
	Path string
	// Type is the entry's type, or a symlink target's type when
	// Options.Type resolved it.
	Type fsys.Type
	// Size is the byte size of regular files under SortByMtime, else -1.
	Size int64
	// Mtime is the modification time in Unix nanoseconds under SortByMtime,
	// else 0.
	Mtime int64
}

// Result is the outcome of a [Glob] search.
type Result struct {
	// Matches are the returned matches, at most Options.Limit of them.
	Matches []Match
	// Total counts every match before Limit applied.
	Total int
	// Truncated reports that Limit dropped matches (Total > len(Matches)).
	Truncated bool
}

// Glob returns the entries below root matching opts.Pattern. It errors on an
// empty or malformed pattern, when root is not a readable directory, or when
// ctx ends the walk; unreadable directories below root are skipped.
func Glob(ctx context.Context, root string, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	sel, err := NewSelector(opts.Pattern, opts.Anchored)
	if err != nil {
		return Result{}, fmt.Errorf("glob: %w", err)
	}
	wo := opts.Walk
	sel.Bound(&wo)
	wo.Detail = fsys.Minimal
	if opts.SortByMtime {
		wo.Detail = fsys.Full
	}
	// Mtime ranking orders everything itself; path order comes from the
	// walker's parallel per-directory sort.
	wo.Sorted = !opts.SortByMtime

	f := filter{ctx: ctx, sel: sel, typ: opts.Type, full: opts.SortByMtime}
	wo.Filter = f.visit
	entries, err := walk.Collect(ctx, root, wo)
	if err != nil {
		return Result{}, err
	}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	res := Result{Total: len(entries)}
	if opts.SortByMtime {
		entries = newest(ctx, entries, opts.Limit)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if opts.Limit > 0 && len(entries) > opts.Limit {
		entries = entries[:opts.Limit]
		res.Truncated = true
	}
	res.Matches = make([]Match, len(entries))
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		res.Matches[i] = Match{Path: e.Path, Type: e.Type, Size: e.Size, Mtime: e.Mtime}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return res, nil
}

// filter is the single selection stage of a search, run concurrently by
// the walk's workers as its Options.Filter.
type filter struct {
	ctx context.Context
	sel *Selector
	typ fsys.Type
	// full is set when entries carry size and mtime (Full detail).
	full bool
}

func (f *filter) visit(w *walk.Worker, e *walk.Entry) walk.Action {
	if f.ctx.Err() != nil {
		return walk.Stop
	}
	if f.sel.Matches(e) && f.wanted(w, e) {
		return walk.Continue
	}
	if f.sel.Excludes(e) {
		return walk.SkipDir
	}
	return walk.Skip
}

// wanted applies the type filter to a matching entry. A symlink kept for
// its target's type is rewritten to describe the target.
func (f *filter) wanted(w *walk.Worker, e *walk.Entry) bool {
	if f.typ == 0 || e.Type == f.typ {
		return true
	}
	if e.Type != fsys.Symlink || (f.typ != fsys.File && f.typ != fsys.Dir) {
		return false
	}
	if f.ctx.Err() != nil {
		return false
	}
	info, err := w.Session().Stat(w.Abs(e), true)
	if err != nil || info.Type != f.typ || f.ctx.Err() != nil {
		return false // dangling, unreadable, or the other type
	}
	e.Type = info.Type
	if f.full {
		e.Mtime = info.Stamp.Mtime
		e.Size = -1
		if info.Type == fsys.File {
			e.Size = info.Size
		}
	}
	return true
}

// newerFirst orders entries by mtime descending, then path ascending, so
// ranking is total and deterministic. Mtimes compare at millisecond
// granularity for ranking: entries
// whose reported mtimes are equal are ordered by path, not by sub-millisecond
// differences the caller cannot see.
func newerFirst(a, b *walk.Entry) int {
	if c := cmp.Compare(b.Mtime/1e6, a.Mtime/1e6); c != 0 {
		return c
	}
	return strings.Compare(a.Path, b.Path)
}

// newest reorders es newest first and returns it. With 0 < limit < len(es)
// only the first limit entries are ranked, found with a bounded heap in
// O(n log limit); the rest of the slice is left unordered.
func newest(ctx context.Context, es []walk.Entry, limit int) []walk.Entry {
	if ctx.Err() != nil {
		return es
	}
	if limit <= 0 || limit >= len(es) {
		slices.SortFunc(es, func(a, b walk.Entry) int { return newerFirst(&a, &b) })
		return es
	}
	h := es[:limit] // heap with the worst-ranked kept entry on top
	for i := limit/2 - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			return es
		}
		siftDown(h, i)
	}
	for i := limit; i < len(es); i++ {
		if ctx.Err() != nil {
			return es
		}
		if newerFirst(&es[i], &h[0]) < 0 {
			h[0], es[i] = es[i], h[0]
			siftDown(h, 0)
		}
	}
	if ctx.Err() != nil {
		return es
	}
	slices.SortFunc(h, func(a, b walk.Entry) int { return newerFirst(&a, &b) })
	return es
}

// siftDown restores the worst-on-top heap property below h[i].
func siftDown(h []walk.Entry, i int) {
	for {
		worst := i
		for _, c := range [2]int{2*i + 1, 2*i + 2} {
			if c < len(h) && newerFirst(&h[c], &h[worst]) > 0 {
				worst = c
			}
		}
		if worst == i {
			return
		}
		h[i], h[worst] = h[worst], h[i]
		i = worst
	}
}
