package grep

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/walk"
)

type cancelFS struct {
	fsys.FS
	started chan struct{}
	release chan struct{}
	mapped  bool
	handles atomic.Int64
	active  atomic.Int64
	unmaps  atomic.Int64
}

func (f *cancelFS) NewSession() fsys.Session {
	return &cancelSession{Session: f.FS.NewSession(), f: f}
}

type cancelSession struct {
	fsys.Session
	f *cancelFS
}

func (s *cancelSession) OpenDir(path string) (fsys.Handle, error) {
	h, err := s.Session.OpenDir(path)
	if err == nil {
		s.f.handles.Add(1)
	}
	return h, err
}

func (s *cancelSession) OpenChild(parent fsys.Handle, name string) (fsys.Handle, error) {
	h, err := s.Session.OpenChild(parent, name)
	if err == nil {
		s.f.handles.Add(1)
	}
	return h, err
}

func (s *cancelSession) OpenFile(dir fsys.Handle, name string) (fsys.Handle, fsys.FileInfo, error) {
	h, info, err := s.Session.OpenFile(dir, name)
	if err == nil {
		s.f.handles.Add(1)
		if s.f.mapped {
			// Exercise the mapping branch without allocating a large file.
			info.Size = mapMin
		}
	}
	return h, info, err
}

func (s *cancelSession) Close(h fsys.Handle) {
	s.Session.Close(h)
	s.f.handles.Add(-1)
}

func (s *cancelSession) blocked() {
	s.f.active.Add(1)
	defer s.f.active.Add(-1)
	s.f.started <- struct{}{}
	<-s.f.release
}

func (s *cancelSession) Read(h fsys.Handle, buf []byte, limit int64) ([]byte, error) {
	s.blocked()
	return s.Session.Read(h, buf, limit)
}

func (s *cancelSession) Map(fsys.Handle, int64) ([]byte, error) {
	s.blocked()
	return []byte("needle\n"), nil
}

func (s *cancelSession) Unmap([]byte) { s.f.unmaps.Add(1) }

func TestGrepCancellationJoinsFileWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		root    string
		limit   int
		readers int
		mapped  bool
	}{
		{name: "single-read", root: "/a/file.txt", readers: 1},
		{name: "single-map", root: "/a/file.txt", readers: 1, mapped: true},
		{name: "unlimited", root: "/", readers: 1},
		{name: "ordered", root: "/", limit: 1, readers: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &cancelFS{
				FS: fsys.FromIOFS(fstest.MapFS{
					"a/file.txt": &fstest.MapFile{Data: []byte("no match\n")},
					"b/file.txt": &fstest.MapFile{Data: []byte("no match\n")},
				}),
				started: make(chan struct{}, tc.readers),
				release: make(chan struct{}),
				mapped:  tc.mapped,
			}
			var release sync.Once
			defer release.Do(func() { close(f.release) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type outcome struct {
				res Result
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				res, err := Grep(ctx, tc.root, Options{Pattern: "needle", MaxCount: tc.limit, Walk: walk.Options{FS: f, Workers: 2}})
				done <- outcome{res, err}
			}()
			for range tc.readers {
				select {
				case <-f.started:
				case got := <-done:
					t.Fatalf("returned before entering file work: %v", got.err)
				case <-t.Context().Done():
					t.Fatal("file work never started")
				}
			}
			cancel()
			select {
			case got := <-done:
				t.Fatalf("returned without joining blocked file work: %v", got.err)
			default:
			}
			release.Do(func() { close(f.release) })
			got := <-done
			if !errors.Is(got.err, context.Canceled) {
				t.Fatalf("got %v, want context.Canceled", got.err)
			}
			if got.res.TotalMatches != 0 || len(got.res.Matches) != 0 {
				t.Fatalf("canceled scan returned matches: %+v", got.res)
			}
			if n := f.active.Load(); n != 0 {
				t.Errorf("%d file operations still running", n)
			}
			if n := f.handles.Load(); n != 0 {
				t.Errorf("%d handles still open", n)
			}
			if tc.mapped && f.unmaps.Load() != int64(tc.readers) {
				t.Errorf("unmapped %d mappings, want %d", f.unmaps.Load(), tc.readers)
			}
		})
	}
}

type noSetupFS struct{ fsys.FS }

func (noSetupFS) Abs(string) (string, error) { panic("canceled grep touched filesystem") }

func TestGrepAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Cancellation takes precedence even over invalid-pattern compilation.
	_, err := Grep(ctx, "/", Options{Pattern: "[", Walk: walk.Options{FS: noSetupFS{}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// pollContext pauses a chosen cancellation checkpoint so the test can cancel
// inside a no-match iterator without racing a timer against its execution.
// Each instance is only polled by one scanning goroutine.
type pollContext struct {
	context.Context
	at      int
	calls   int
	reached chan struct{}
	resume  chan struct{}
}

func (c *pollContext) Err() error {
	c.calls++
	if c.calls == c.at {
		close(c.reached)
		<-c.resume
	}
	return c.Context.Err()
}

func TestNoMatchIteratorsPollCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern string
		pref    bool
	}{
		{name: "accelerated-dfa", pattern: `\p{Greek}`},
		{name: "transition-dfa", pattern: `^[a-z]+[0-9]$`},
		{name: "literal-prefilter", pattern: "needle", pref: true},
		{name: "folded-prefilter", pattern: "(?i)needle", pref: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := compilePattern(tc.pattern, false, false, false)
			if err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &pollContext{Context: base, at: 2, reached: make(chan struct{}), resume: make(chan struct{})}
			if tc.pref {
				ctx.at = 3 // find, first candidate chunk, second candidate chunk
			}
			buf := []byte(strings.Repeat("a", 2*scanChunk+1))
			done := make(chan bool, 1)
			go func() {
				if tc.pref {
					var st prefState
					st.reset()
					done <- c.pre.find(ctx, buf, 0, &st) >= 0
				} else {
					_, _, matched := newDFA(c.prog).next(ctx, buf, 0)
					done <- matched
				}
			}()
			select {
			case <-ctx.reached:
			case <-done:
				t.Fatal("no-match iterator completed without polling inside its scan")
			}
			cancel()
			close(ctx.resume)
			if <-done {
				t.Fatal("canceled no-match iterator reported a match")
			}
		})
	}
}

type scanCancelFS struct {
	fsys.FS
	ctx *pollContext
}

func (f *scanCancelFS) NewSession() fsys.Session {
	return &scanCancelSession{Session: f.FS.NewSession(), ctx: f.ctx}
}

type scanCancelSession struct {
	fsys.Session
	ctx *pollContext
}

func (s *scanCancelSession) Read(h fsys.Handle, buf []byte, limit int64) ([]byte, error) {
	data, err := s.Session.Read(h, buf, limit)
	// Arm after I/O: searchData, scan and its loop, blocks.next and its
	// loop, then prefilter.find and the first two candidate chunks poll Err.
	s.ctx.at = s.ctx.calls + 8
	return data, err
}

func TestSingleFileNoMatchScanCancellation(t *testing.T) {
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &pollContext{Context: base, reached: make(chan struct{}), resume: make(chan struct{})}
	f := &scanCancelFS{
		FS: fsys.FromIOFS(fstest.MapFS{
			"file.txt": {Data: []byte(strings.Repeat("a", 2*scanChunk+1))},
		}),
		ctx: ctx,
	}
	done := make(chan error, 1)
	go func() {
		_, err := Grep(ctx, "/file.txt", Options{Pattern: "needle", Walk: walk.Options{FS: f}})
		done <- err
	}()
	select {
	case <-ctx.reached:
	case err := <-done:
		t.Fatalf("no-match grep completed without polling inside the scan: %v", err)
	}
	cancel()
	close(ctx.resume)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled no-match scan returned %v", err)
	}
}

func TestPrefilterChunkBoundaries(t *testing.T) {
	for _, pattern := range []string{"needle", "(?i)needle", "needle|OTHER"} {
		c, err := compilePattern(pattern, false, false, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, offset := range []int{scanChunk - 3, scanChunk + 3, 2*scanChunk + 3} {
			buf := []byte(strings.Repeat("a", offset) + "needle")
			var st prefState
			st.reset()
			if got := c.pre.find(t.Context(), buf, 0, &st); got != offset {
				t.Errorf("%s at %d: got %d", pattern, offset, got)
			}
		}
	}
}
