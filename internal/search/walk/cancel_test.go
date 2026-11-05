package walk

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
)

type cancelWalkFS struct {
	fsys.FS
	phase   string
	started chan struct{}
	release chan struct{}
	once    sync.Once
	handles atomic.Int64
	lists   atomic.Int64
}

func (f *cancelWalkFS) NewSession() fsys.Session {
	return &cancelWalkSession{Session: f.FS.NewSession(), f: f}
}

func (f *cancelWalkFS) block(phase string) {
	if f.phase == phase {
		f.once.Do(func() {
			close(f.started)
			<-f.release
		})
	}
}

type cancelWalkSession struct {
	fsys.Session
	f *cancelWalkFS
}

func (s *cancelWalkSession) OpenDir(path string) (fsys.Handle, error) {
	h, err := s.Session.OpenDir(path)
	if err == nil {
		s.f.handles.Add(1)
	}
	s.f.block("open")
	return h, err
}

func (s *cancelWalkSession) OpenChild(parent fsys.Handle, name string) (fsys.Handle, error) {
	h, err := s.Session.OpenChild(parent, name)
	if err == nil {
		s.f.handles.Add(1)
	}
	return h, err
}

func (s *cancelWalkSession) OpenFile(dir fsys.Handle, name string) (fsys.Handle, fsys.FileInfo, error) {
	h, info, err := s.Session.OpenFile(dir, name)
	if err == nil {
		s.f.handles.Add(1)
	}
	return h, info, err
}

func (s *cancelWalkSession) Close(h fsys.Handle) {
	s.Session.Close(h)
	s.f.handles.Add(-1)
}

func (s *cancelWalkSession) List(h fsys.Handle, detail fsys.Detail, children uint32) (*fsys.Listing, error) {
	s.f.lists.Add(1)
	ls, err := s.Session.List(h, detail, children)
	s.f.block("list")
	return ls, err
}

func (s *cancelWalkSession) Read(h fsys.Handle, buf []byte, limit int64) ([]byte, error) {
	data, err := s.Session.Read(h, buf, limit)
	s.f.block("ignore")
	return data, err
}

func TestWalkCancellationJoinsFilesystemWork(t *testing.T) {
	isolateGit(t)
	for _, phase := range []string{"open", "list", "ignore"} {
		t.Run(phase, func(t *testing.T) {
			f := &cancelWalkFS{
				FS: fsys.FromIOFS(fstest.MapFS{
					".git/config":     {},
					".ignore":         {Data: []byte("*.log\n")},
					"repo/a/file.txt": {},
					"repo/b/file.txt": {},
				}),
				phase:   phase,
				started: make(chan struct{}),
				release: make(chan struct{}),
			}
			var release sync.Once
			defer release.Do(func() { close(f.release) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			var visits atomic.Int64
			go func() {
				done <- Walk(ctx, "/repo", Options{FS: f, Workers: 2, Gitignore: phase == "ignore"}, func(*Worker, *Entry) Action {
					visits.Add(1)
					return Continue
				})
			}()
			select {
			case <-f.started:
			case err := <-done:
				t.Fatalf("returned before %s hook: %v", phase, err)
			}
			cancel()
			select {
			case err := <-done:
				t.Fatalf("returned while %s was still blocked: %v", phase, err)
			default:
			}
			release.Do(func() { close(f.release) })
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want context.Canceled", err)
			}
			if visits.Load() != 0 {
				t.Errorf("visited %d entries after cancellation", visits.Load())
			}
			if phase != "list" && f.lists.Load() != 0 {
				t.Errorf("listed %d directories after setup cancellation", f.lists.Load())
			}
			if f.handles.Load() != 0 {
				t.Errorf("%d handles still open", f.handles.Load())
			}
		})
	}
}

func TestWalkCancellationDrainsQueuedDirectories(t *testing.T) {
	f := &cancelWalkFS{FS: fsys.FromIOFS(fstest.MapFS{
		"a/file.txt": {},
		"b/file.txt": {},
		"c/file.txt": {},
	})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	visits := 0
	err := Walk(ctx, "/", Options{FS: f, Workers: 1}, func(*Worker, *Entry) Action {
		visits++
		if visits == 2 {
			cancel() // the first directory already holds a queued parent ref
		}
		return Continue
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if visits != 2 {
		t.Errorf("visited %d entries, want 2", visits)
	}
	if f.lists.Load() != 1 || f.handles.Load() != 0 {
		t.Errorf("lists=%d open handles=%d, want 1 and 0", f.lists.Load(), f.handles.Load())
	}
}

type noSetupWalkFS struct{ fsys.FS }

func (noSetupWalkFS) Abs(string) (string, error) { panic("canceled walk touched filesystem") }

func TestWalkAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	opts := Options{FS: noSetupWalkFS{}}
	if err := Walk(ctx, "/", opts, func(*Worker, *Entry) Action { return Continue }); !errors.Is(err, context.Canceled) {
		t.Errorf("Walk got %v, want context.Canceled", err)
	}
	if _, err := Collect(ctx, "/", opts); !errors.Is(err, context.Canceled) {
		t.Errorf("Collect got %v, want context.Canceled", err)
	}
}
