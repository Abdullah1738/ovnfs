// Package grep searches file contents for a regular expression using the
// shared walker and filesystem layer.
//
// Patterns use the Rust regex dialect (see regex.go for what Go can and
// cannot reproduce). Matching is line oriented: a match is a line (or, in
// multiline mode, a block of lines) containing a match, reported with
// optional context lines. Most patterns carry a literal every match must
// contain; scanning for it with SIMD byte search leaves only candidate
// lines to check (literal.go), and a lazily built DFA checks lines in one
// pass (dfa.go). Multiline searches use Go's regexp. Built with
// GOEXPERIMENT=simd, the literal, DFA-skip, and newline searches use the
// portable vectors of package simd (simd.go); otherwise byte loops and the
// bytes package (nosimd.go).
//
// Without MaxCount, files are read and searched inside one parallel walk.
// With MaxCount, the candidates are collected in path order and searched in
// parallel from the front, stopping once the first MaxCount (+Offset)
// results are settled. Either way results are in path order, then line
// order, and identical from run to run. With [Options.Remote], files whose
// bytes only another machine holds are scanned there in batches ([Remote]),
// and only their results cross the link.
//
//	res, err := grep.Grep(ctx, "/repo", grep.Options{
//		Pattern: `impl\s+\w+ for`,
//		Glob:    "*.rs",
//		Before:  1, After: 1,
//		Walk:    walk.Options{Gitignore: true, SkipGit: true, SkipNodeModules: true},
//	})
package grep

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/walk"
)

// DefaultMaxFileSize is how much of a file is searched when
// Options.MaxFileSize is zero.
const DefaultMaxFileSize = 4 << 20

// mapMin is the smallest window searched through a mapping rather than a
// read. Below it, mmap, munmap, and page faults cost more than read's copy;
// from 2 MiB up, mapping scanned files 1.2–1.7× faster than reading them
// (Apple M4 Max, warm page cache, 1 and 16 workers).
const mapMin = 2 << 20

// Mode selects what a search reports.
type Mode uint8

const (
	// Content reports matching lines with their context.
	Content Mode = iota
	// FilesWithMatches reports one entry per matching file.
	FilesWithMatches
	// Count reports one entry per matching file with its match count.
	Count
)

// Options configures a [Grep] search.
type Options struct {
	// Pattern is a regular expression in the Rust dialect, or literal
	// text with Literal.
	Pattern string
	// Literal searches for Pattern as plain text.
	Literal bool
	// IgnoreCase matches with Unicode simple case folding.
	IgnoreCase bool
	// Multiline lets matches span lines ('\n' may match; ^ and $ still
	// anchor at line boundaries). Matches on the same or adjacent lines are
	// reported as one block.
	Multiline bool
	// Glob, when set, scopes the search ([glob.NewScope]): files whose
	// root-relative path matches it, read as the glob tool reads patterns
	// (a slash-free glob matches at any depth unless Anchored; one naming
	// node_modules also searches node_modules), plus every file under a
	// directory it matches.
	Glob     string
	Anchored bool
	// Exclude leaves out files and whole directories matching any of its
	// globs ([glob.NewExclude]: slash-free globs match at any depth, a
	// leading '/' anchors to the root).
	Exclude []string
	// Type, when set, keeps files of one type from the table in filter.go,
	// such as "rust" or "ts"; any other value is an extension or file name.
	Type string
	// Mode selects the report; see [Mode].
	Mode Mode
	// Before and After are context lines around each match (Content only).
	Before, After int
	// MaxColumns, when positive, fits reported lines longer than that many
	// bytes into MaxColumns bytes: context lines keep their first
	// MaxColumns-3 bytes plus "...", matching lines a window around their
	// first match marked with "..." on each cut side (see [Match.Column]).
	MaxColumns int
	// Offset skips that many results (matches in Content and Count mode,
	// files in FilesWithMatches mode) in path order.
	Offset int
	// MaxCount, when positive, returns at most that many results after
	// Offset; the search stops as soon as they are known.
	MaxCount int
	// MaxCountPerFile, when positive, keeps at most that many matches per
	// file (Content only), so one dense file cannot use up MaxCount.
	MaxCountPerFile int
	// MaxFileSize bounds how many leading bytes of a file are searched; zero
	// means DefaultMaxFileSize. Longer files are searched over that window;
	// those that cannot be read count in Result.SkippedOversized.
	MaxFileSize int64
	// Walk carries the traversal options (filesystem, hidden files, ignore
	// files, .git and node_modules pruning, workers, listing cache). Grep
	// sets Detail, Sorted, and Filter itself and leaves FollowLinks as given
	// (the default does not follow links).
	Walk walk.Options
	// Remote, when set, scans the files only another machine holds on that
	// machine ([Remote]).
	Remote Remote
}

// ContextLine is a line reported around a match.
type ContextLine struct {
	// Line is the 1-based line number.
	Line int `json:"line"`
	// Text is the line without trailing whitespace (see [Match.Text]).
	Text string `json:"text"`
}

// Match is one result entry.
type Match struct {
	// Path is the file, relative to the root ('/'-separated); for a root
	// that is a file, its absolute path.
	Path string `json:"path,omitempty"`
	// Line is the 1-based number of the matching line (of a block's first
	// line in multiline mode); 0 outside Content mode.
	Line int `json:"line,omitempty"`
	// Text is the matching line (a multiline block's lines joined by '\n')
	// with trailing whitespace trimmed and invalid UTF-8 replaced, cut to
	// MaxColumns; empty outside Content mode.
	Text string `json:"text,omitempty"`
	// Before and After are context lines (Content only).
	Before []ContextLine `json:"before,omitempty"`
	After  []ContextLine `json:"after,omitempty"`
	// Truncated reports that MaxColumns cut Text.
	Truncated bool `json:"truncated,omitempty"`
	// Column is, when MaxColumns cut Text, the 1-based character column of
	// the first match within the (untruncated) line or block; else 0.
	Column int `json:"column,omitempty"`
	// Count is the file's match count in Count mode, else 0.
	Count int `json:"count,omitempty"`
}

// Result is the outcome of a [Grep] search.
type Result struct {
	// Matches are the selected results in path, then line order.
	Matches []Match
	// TotalMatches counts the matches found in the searched files (in
	// FilesWithMatches mode, the matching files). It is exact unless a cap
	// stopped the search: a file's scan stops one match past
	// MaxCountPerFile or Offset+MaxCount, and MaxCount stops the search
	// after the file that settles the results.
	TotalMatches int
	// FilesWithMatches counts searched files with at least one match.
	FilesWithMatches int
	// FilesSearched counts files read and searched (binary files included).
	FilesSearched int
	// LimitReached reports that results were left out: MaxCount results
	// were returned (more may exist past the searched files), or a file had
	// more matches than MaxCountPerFile.
	LimitReached bool
	// SkippedOversized counts files larger than MaxFileSize that could not
	// be read (pi-walker's skipped_oversized); readable ones are searched
	// over their window like any file.
	SkippedOversized int
}

// Grep searches the files below root (or root itself, if it is a file).
// It errors on an empty or unsupported pattern, a malformed glob, a root
// that does not exist, or when ctx ends the search; unreadable files and
// directories below root are skipped.
func Grep(ctx context.Context, root string, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	q := opts.query()
	c, err := q.compile()
	if err != nil {
		return Result{}, err
	}
	filter, err := newFileFilter(&opts)
	if err != nil {
		return Result{}, err
	}
	s := &search{opts: &opts, q: q, c: c, filter: filter, params: q.params()}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	fs := opts.Walk.Filesystem()
	abs, err := fs.Abs(root)
	if cancelErr := ctx.Err(); cancelErr != nil {
		return Result{}, cancelErr
	}
	if err != nil {
		return Result{}, err
	}
	info, err := fs.NewSession().Stat(abs, true)
	if cancelErr := ctx.Err(); cancelErr != nil {
		return Result{}, cancelErr
	}
	if err != nil {
		return Result{}, fmt.Errorf("grep: %w", err)
	}
	switch info.Type {
	case fsys.File:
		return s.file(ctx, abs)
	case fsys.Dir:
	default:
		return Result{}, nil
	}

	wo := opts.Walk
	wo.Detail, wo.Sorted, wo.Filter = fsys.Minimal, false, nil
	filter.tighten(&wo)
	if opts.MaxCount > 0 {
		return s.ordered(ctx, root, wo)
	}
	return s.parallel(ctx, root, wo)
}

// search is one Grep call's shared, read-only state.
type search struct {
	opts   *Options
	q      Query
	c      *compiled
	filter *fileFilter
	params scanParams
}

// fileHit is a searched file with matches and its counts.
type fileHit struct {
	path    string
	matches []Match // Content only
	count   int
	more    bool
}

// tally is one worker's per-file bookkeeping.
type tally struct {
	hits      []fileHit
	searched  int
	oversized int
}

// searchFile searches the open file h (of total size size) and closes it:
// windows of at least mapMin bytes are mapped, smaller ones read into the
// scanner's buffer. It returns the file's units toward MaxCount (see
// searchData).
func (s *search) searchFile(ctx context.Context, sc *scanner, t *tally, sess fsys.Session, h fsys.Handle, path string, size int64) int {
	defer sess.Close(h)
	if ctx.Err() != nil {
		return 0
	}
	if min(size, s.q.Limit) >= mapMin {
		if data, err := sess.Map(h, s.q.Limit); err == nil {
			defer sess.Unmap(data)
			return s.searchData(ctx, sc, t, path, data, size)
		}
		if ctx.Err() != nil {
			return 0
		}
	}
	data, err := sess.Read(h, sc.buf, s.q.Limit)
	sc.buf = data
	if err != nil {
		if size > s.q.Limit {
			t.oversized++
		}
		return 0
	}
	return s.searchData(ctx, sc, t, path, data, size)
}

// searchData scans one file's content (of total size size) and records the
// outcome in t. A file that shrinks under a mapping mid-scan is skipped,
// like one that fails to read. path is volatile and copied only on a
// match. It returns the file's units toward MaxCount (see record).
func (s *search) searchData(ctx context.Context, sc *scanner, t *tally, path string, data []byte, size int64) int {
	if ctx.Err() != nil {
		return 0
	}
	lo := len(sc.out)
	var o Outcome
	if !fsys.Guard(data, func() { o = sc.outcome(ctx, data, s.params) }) || ctx.Err() != nil {
		sc.out = sc.out[:lo]
		return 0
	}
	if o.Count > 0 {
		path = sc.clone(path)
	}
	return s.record(t, path, size, o)
}

// record adds the outcome of the file at path (of total size size; path is
// kept as is) to t: an unread file counts only toward oversized ones when
// larger than the window, like a failed local read. It returns the file's
// units toward MaxCount: kept matches in Content mode, matches in Count
// mode, and one matching file in FilesWithMatches mode.
func (s *search) record(t *tally, path string, size int64, o Outcome) int {
	if !o.Searched {
		if size > s.q.Limit {
			t.oversized++
		}
		return 0
	}
	t.searched++
	if o.Count == 0 {
		return 0
	}
	t.hits = append(t.hits, fileHit{path: path, matches: o.Matches, count: o.Count, more: o.More})
	switch s.opts.Mode {
	case Content:
		return len(o.Matches)
	case Count:
		return o.Count
	}
	return 1
}

// file searches a root that is a regular file; matches carry its absolute
// path. The type filter applies to its name; the glob does not (the file
// was named explicitly).
func (s *search) file(ctx context.Context, abs string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	name := abs[strings.LastIndexAny(abs, `/\`)+1:]
	if !s.filter.typ.matches(name) {
		return Result{}, nil
	}
	fs := s.opts.Walk.Filesystem()
	var t tally
	if b, ok := s.locate(abs); ok {
		s.scanBatch(ctx, fs, []remoteFile{{path: abs, abs: abs, blob: b}}, func(int) *tally { return &t })
	} else {
		sess := fs.NewSession()
		h, info, err := sess.OpenFile(fsys.Handle{}, abs)
		if err != nil {
			return Result{}, ctx.Err()
		}
		s.searchFile(ctx, newScanner(s.c), &t, sess, h, abs, info.Size)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	res := s.result(ctx, []*tally{&t}, false)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return res, nil
}

// parallel searches every candidate inside one parallel walk; files are
// opened relative to the directory handle the walk holds open.
func (s *search) parallel(ctx context.Context, root string, wo walk.Options) (Result, error) {
	n := wo.Parallelism()
	scanners := make([]*scanner, n)
	tallies := make([]*tally, n)
	for i := range n {
		scanners[i], tallies[i] = newScanner(s.c), &tally{}
	}
	remote := s.newRemoteScans(ctx, wo.Filesystem())
	err := walk.Walk(ctx, root, wo, func(w *walk.Worker, e *walk.Entry) walk.Action {
		if ctx.Err() != nil {
			return walk.Stop
		}
		act := s.filter.visit(e)
		if act != walk.Continue || e.Type != fsys.File {
			return act
		}
		if remote != nil && remote.add(w.Abs(e), e.Path) {
			return walk.Continue
		}
		if h, info, err := w.OpenFile(e); err == nil {
			s.searchFile(ctx, scanners[w.ID()], tallies[w.ID()], w.Session(), h, e.Path, info.Size)
		}
		return walk.Continue
	})
	if remote != nil {
		tallies = append(tallies, remote.wait()...)
	}
	if err != nil {
		return Result{}, err
	}
	res := s.result(ctx, tallies, false)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return res, nil
}

// ordered collects the candidates in path order and searches them from the
// front in parallel: workers claim files in order, and once the finished
// prefix holds Offset+MaxCount units the files after it are abandoned.
// Only that prefix contributes, so results and totals do not depend on
// scheduling. Files the Remote holds are scanned there in path-ordered
// batches beside the workers; a batch wholly past the prefix is canceled.
func (s *search) ordered(ctx context.Context, root string, wo walk.Options) (Result, error) {
	wo.Sorted = true
	wo.Filter = func(_ *walk.Worker, e *walk.Entry) walk.Action { return s.filter.visit(e) }
	files, err := walk.Collect(ctx, root, wo)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	fs := wo.Filesystem()
	abs, err := fs.Abs(root)
	if cancelErr := ctx.Err(); cancelErr != nil {
		return Result{}, cancelErr
	}
	if err != nil {
		return Result{}, err
	}
	sep := string(fs.Separator())
	if !strings.HasSuffix(abs, sep) {
		abs += sep
	}

	need := max(0, s.opts.Offset) + s.opts.MaxCount
	type slot struct {
		tally  tally // this file's outcome alone
		units  int
		done   bool
		remote bool // scanned by a remote batch, not the workers
	}
	slots := make([]slot, len(files))
	var remote []remoteFile
	var remoteAt []int // slot of each remote file
	if s.opts.Remote != nil {
		for i := range files {
			p := abs + files[i].Path
			if sep != "/" {
				p = strings.ReplaceAll(p, "/", sep)
			}
			if b, ok := s.opts.Remote.Locate(p); ok {
				slots[i].remote = true
				remote = append(remote, remoteFile{path: files[i].Path, abs: p, blob: b})
				remoteAt = append(remoteAt, i)
			}
		}
	}
	type batch struct {
		first  int // slot of its first file
		cancel context.CancelFunc
	}
	var (
		next     atomic.Int64
		stop     atomic.Int64 // files at or past stop are not needed
		mu       sync.Mutex
		frontier int     // slots[:frontier] are done
		units    int     // units in slots[:frontier]
		batches  []batch // remote batches started
	)
	stop.Store(int64(len(files)))
	// settle records slot i's units and advances the frontier; mu is held.
	settle := func(i, n int) {
		slots[i].units, slots[i].done = n, true
		for frontier < len(slots) && int64(frontier) < stop.Load() && slots[frontier].done {
			units += slots[frontier].units
			frontier++
			if units >= need {
				stop.Store(int64(frontier))
				for _, b := range batches {
					if b.first >= frontier {
						b.cancel()
					}
				}
			}
		}
	}
	workers := min(wo.Parallelism(), max(1, len(files)))
	var wg sync.WaitGroup
	if len(remote) > 0 {
		wg.Go(func() {
			sem := make(chan struct{}, remoteInFlight)
			var scans sync.WaitGroup
			for lo := 0; lo < len(remote) && ctx.Err() == nil; {
				sem <- struct{}{}
				if int64(remoteAt[lo]) >= stop.Load() {
					break
				}
				hi := s.batchEnd(remote, lo)
				part, at := remote[lo:hi], remoteAt[lo:hi]
				bctx, cancel := context.WithCancel(ctx)
				mu.Lock()
				batches = append(batches, batch{first: at[0], cancel: cancel})
				mu.Unlock()
				scans.Go(func() {
					defer func() { <-sem }()
					defer cancel()
					n := s.scanBatch(bctx, fs, part, func(i int) *tally { return &slots[at[i]].tally })
					mu.Lock()
					for i := range part {
						settle(at[i], n[i])
					}
					mu.Unlock()
				})
				lo = hi
			}
			scans.Wait()
		})
	}
	for range workers {
		wg.Go(func() {
			sc := newScanner(s.c)
			sess := fs.NewSession()
			path := []byte(abs)
			for ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if int64(i) >= stop.Load() {
					return
				}
				sl := &slots[i]
				if sl.remote {
					continue
				}
				rel := files[i].Path
				path = append(path[:len(abs)], rel...)
				if sep != "/" {
					path = []byte(strings.ReplaceAll(string(path), "/", sep))
				}
				// The name is only read during the call, so it may alias the
				// reused path buffer.
				name := unsafe.String(unsafe.SliceData(path), len(path))
				n := 0
				if h, info, err := sess.OpenFile(fsys.Handle{}, name); err == nil {
					n = s.searchFile(ctx, sc, &sl.tally, sess, h, rel, info.Size)
				}
				mu.Lock()
				settle(i, n)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	settled := slots[:stop.Load()]
	tallies := make([]*tally, len(settled))
	for i := range settled {
		tallies[i] = &settled[i].tally
	}
	res := s.result(ctx, tallies, true)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return res, nil
}

// result merges per-worker (or per-file) tallies into path order and
// applies Offset and MaxCount. sorted says the tallies are already in path
// order.
func (s *search) result(ctx context.Context, tallies []*tally, sorted bool) Result {
	var res Result
	var hits []fileHit
	for _, t := range tallies {
		if ctx.Err() != nil {
			return Result{}
		}
		res.FilesSearched += t.searched
		res.SkippedOversized += t.oversized
		hits = append(hits, t.hits...)
	}
	if !sorted {
		slices.SortFunc(hits, func(a, b fileHit) int { return cmp.Compare(a.path, b.path) })
	}
	offset, limit := max(0, s.opts.Offset), s.opts.MaxCount
	full := func(emitted int) bool { return limit > 0 && emitted >= limit }
	skipped, emitted := 0, 0
	for _, h := range hits {
		if ctx.Err() != nil {
			return Result{}
		}
		res.FilesWithMatches++
		start := res.TotalMatches
		res.TotalMatches += h.count
		if h.more {
			res.LimitReached = true
		}
		switch s.opts.Mode {
		case Content:
			for _, m := range h.matches {
				if ctx.Err() != nil {
					return Result{}
				}
				if skipped < offset {
					skipped++
					continue
				}
				if full(emitted) {
					break
				}
				m.Path = h.path
				res.Matches = append(res.Matches, m)
				emitted++
			}
		case Count:
			available := h.count - min(h.count, max(0, offset-start))
			if available == 0 || full(emitted) {
				continue
			}
			res.Matches = append(res.Matches, Match{Path: h.path, Count: h.count})
			take := available
			if limit > 0 {
				take = min(available, limit-emitted)
			}
			emitted += take
		case FilesWithMatches:
			if skipped < offset {
				skipped++
				continue
			}
			if full(emitted) {
				continue
			}
			res.Matches = append(res.Matches, Match{Path: h.path})
			emitted++
		}
	}
	if full(emitted) {
		res.LimitReached = true
	}
	return res
}
