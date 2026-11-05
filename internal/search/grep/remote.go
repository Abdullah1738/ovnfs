package grep

import (
	"context"
	"strings"
	"sync"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
)

// Query is what one file's scan matches and reports: the per-file half of
// [Options], which Grep derives from them. It travels over attach links, so
// the machine holding a file's bytes can scan it ([Matcher]) for a search
// running elsewhere ([Remote]).
type Query struct {
	Pattern    string `json:"pattern"`
	Literal    bool   `json:"literal,omitempty"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	Multiline  bool   `json:"multiline,omitempty"`
	// Content reports matching lines with context; otherwise only counts.
	Content    bool `json:"content,omitempty"`
	Before     int  `json:"before,omitempty"`
	After      int  `json:"after,omitempty"`
	MaxColumns int  `json:"max_columns,omitempty"`
	// Cap stops a file's scan after this many matches (0: no cap); with
	// Probe it looks for one more, to report [Outcome.More].
	Cap   int  `json:"cap,omitempty"`
	Probe bool `json:"probe,omitempty"`
	// Limit is how many leading bytes of a file are scanned; zero means
	// DefaultMaxFileSize.
	Limit int64 `json:"limit,omitempty"`
}

// query derives the per-file scan from o.
func (o *Options) query() Query {
	q := Query{
		Pattern: o.Pattern, Literal: o.Literal, IgnoreCase: o.IgnoreCase, Multiline: o.Multiline,
		Content: o.Mode == Content, MaxColumns: o.MaxColumns, Limit: o.MaxFileSize,
	}
	switch o.Mode {
	case Content:
		q.Before, q.After = o.Before, o.After
		q.Cap = max(0, o.MaxCountPerFile)
		if o.MaxCount > 0 {
			window := max(0, o.Offset) + o.MaxCount
			if q.Cap == 0 || window < q.Cap {
				q.Cap = window
			}
		}
		q.Probe = q.Cap > 0
	case FilesWithMatches:
		q.Cap = 1
	}
	return q.normal()
}

// normal clamps q's negative counts to zero and fills in the default Limit.
func (q Query) normal() Query {
	q.Before, q.After, q.MaxColumns, q.Cap = max(0, q.Before), max(0, q.After), max(0, q.MaxColumns), max(0, q.Cap)
	if q.Limit <= 0 {
		q.Limit = DefaultMaxFileSize
	}
	return q
}

// params is what the scanner needs from a normalized q.
func (q *Query) params() scanParams {
	return scanParams{content: q.Content, before: q.Before, after: q.After, maxColumns: q.MaxColumns, cap: q.Cap, probe: q.Probe}
}

// compile compiles q's pattern.
func (q *Query) compile() (*compiled, error) {
	return compilePattern(q.Pattern, q.Literal, q.IgnoreCase, q.Multiline)
}

// Outcome is one file's scan.
type Outcome struct {
	// Searched reports that the file was read and scanned (binary files
	// included); a file that could not be read is skipped, as Grep skips
	// unreadable files.
	Searched bool `json:"searched,omitempty"`
	// Count is the file's match count; the scan stops one past Query.Cap.
	Count int `json:"count,omitempty"`
	// More reports a match past Query.Cap.
	More bool `json:"more,omitempty"`
	// Matches are the kept matches (Content only), Path empty.
	Matches []Match `json:"matches,omitempty"`
}

// outcome scans buf like scan; the outcome's matches alias sc.out.
func (sc *scanner) outcome(ctx context.Context, buf []byte, p scanParams) Outcome {
	lo := len(sc.out)
	_, count, more := sc.scan(ctx, buf, p)
	hi := len(sc.out)
	return Outcome{Searched: true, Count: count, More: more, Matches: sc.out[lo:hi:hi]}
}

// Matcher scans file contents for one [Query], reusing its buffers from
// file to file. It is not safe for concurrent use.
type Matcher struct {
	sc *scanner
	q  Query
}

// NewMatcher compiles q. It errors on an empty or unsupported pattern.
func NewMatcher(q Query) (*Matcher, error) {
	q = q.normal()
	c, err := q.compile()
	if err != nil {
		return nil, err
	}
	return &Matcher{sc: newScanner(c), q: q}, nil
}

// Limit is how many leading bytes of a file Scan searches.
func (m *Matcher) Limit() int64 { return m.q.Limit }

// Scan searches the leading Limit bytes of data, one file's content.
// Outcomes stay valid across later scans.
func (m *Matcher) Scan(ctx context.Context, data []byte) Outcome {
	if int64(len(data)) > m.q.Limit {
		data = data[:m.q.Limit]
	}
	return m.sc.outcome(ctx, data, m.q.params())
}

// Remote scans files whose bytes only another machine holds on that
// machine, so a search moves results instead of file contents. With
// [Options.Remote] set, Grep asks it about every candidate file, scans the
// files it locates there in batches and the others here; a batch the remote
// fails to scan is read through the filesystem instead.
type Remote interface {
	// Locate reports the blob behind the regular file at path (absolute in
	// the walked filesystem) when only the other machine holds its bytes.
	// path is only valid during the call. Walk workers call it concurrently.
	Locate(path string) (Blob, bool)
	// Scan scans blobs with q on the other machine and returns one outcome
	// per blob, in order.
	Scan(ctx context.Context, q Query, blobs []Blob) ([]Outcome, error)
}

// Blob is a file's content on the machine holding it.
type Blob struct {
	ID   string // content id
	Path string // where that machine finds it, as a hint
	Size int64  // full size in bytes
}

// Remote batching: files and scanned bytes per Remote.Scan call, and calls
// in flight per search.
const (
	remoteBatchFiles = 512
	remoteBatchBytes = 32 << 20
	remoteInFlight   = 4
)

// locate asks the Remote, if any, about the file at abs.
func (s *search) locate(abs string) (Blob, bool) {
	if s.opts.Remote == nil {
		return Blob{}, false
	}
	return s.opts.Remote.Locate(abs)
}

// remoteFile is a candidate a Remote located.
type remoteFile struct {
	path string // reported path
	abs  string // path in the walked filesystem, to read it here instead
	blob Blob
}

// batchEnd returns the end of the batch of files starting at lo.
func (s *search) batchEnd(files []remoteFile, lo int) int {
	hi, bytes := lo, int64(0)
	for hi < len(files) && hi-lo < remoteBatchFiles && (hi == lo || bytes < remoteBatchBytes) {
		bytes += min(files[hi].blob.Size, s.q.Limit)
		hi++
	}
	return hi
}

// scanBatch scans files on the remote, or reads them through fs when the
// remote fails, recording each file in tallyOf(i). It returns each file's
// units toward MaxCount (see [search.record]).
func (s *search) scanBatch(ctx context.Context, fs fsys.FS, files []remoteFile, tallyOf func(i int) *tally) []int {
	units := make([]int, len(files))
	blobs := make([]Blob, len(files))
	for i, f := range files {
		blobs[i] = f.blob
	}
	outs, err := s.opts.Remote.Scan(ctx, s.q, blobs)
	if ctx.Err() != nil {
		return units
	}
	if err == nil && len(outs) == len(files) {
		for i, o := range outs {
			units[i] = s.record(tallyOf(i), files[i].path, files[i].blob.Size, o)
		}
		return units
	}
	sc, sess := newScanner(s.c), fs.NewSession()
	for i, f := range files {
		if ctx.Err() != nil {
			break
		}
		if h, info, err := sess.OpenFile(fsys.Handle{}, f.abs); err == nil {
			units[i] = s.searchFile(ctx, sc, tallyOf(i), sess, h, f.path, info.Size)
		}
	}
	return units
}

// remoteScans batches the files Options.Remote locates during a parallel
// walk and scans each batch as it fills, remoteInFlight at a time.
type remoteScans struct {
	s       *search
	ctx     context.Context
	fs      fsys.FS
	sem     chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	batch   []remoteFile
	bytes   int64
	tallies []*tally
}

// newRemoteScans returns nil when the search has no Remote.
func (s *search) newRemoteScans(ctx context.Context, fs fsys.FS) *remoteScans {
	if s.opts.Remote == nil {
		return nil
	}
	return &remoteScans{s: s, ctx: ctx, fs: fs, sem: make(chan struct{}, remoteInFlight)}
}

// add takes the file at abs (reported as path; both volatile) when the
// remote holds it.
func (rs *remoteScans) add(abs, path string) bool {
	b, ok := rs.s.locate(abs)
	if !ok {
		return false
	}
	rs.mu.Lock()
	rs.batch = append(rs.batch, remoteFile{path: strings.Clone(path), abs: strings.Clone(abs), blob: b})
	rs.bytes += min(b.Size, rs.s.q.Limit)
	var full []remoteFile
	if len(rs.batch) >= remoteBatchFiles || rs.bytes >= remoteBatchBytes {
		full, rs.batch, rs.bytes = rs.batch, nil, 0
	}
	rs.mu.Unlock()
	if full != nil {
		rs.dispatch(full)
	}
	return true
}

// dispatch scans one batch in the background.
func (rs *remoteScans) dispatch(files []remoteFile) {
	rs.wg.Go(func() {
		select {
		case rs.sem <- struct{}{}:
		case <-rs.ctx.Done():
			return
		}
		defer func() { <-rs.sem }()
		t := &tally{}
		rs.s.scanBatch(rs.ctx, rs.fs, files, func(int) *tally { return t })
		rs.mu.Lock()
		rs.tallies = append(rs.tallies, t)
		rs.mu.Unlock()
	})
}

// wait scans the last partial batch and returns every batch's tally once
// all are done.
func (rs *remoteScans) wait() []*tally {
	rs.mu.Lock()
	last := rs.batch
	rs.batch, rs.bytes = nil, 0
	rs.mu.Unlock()
	if len(last) > 0 {
		rs.dispatch(last)
	}
	rs.wg.Wait()
	return rs.tallies
}
