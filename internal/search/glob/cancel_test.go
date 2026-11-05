package glob

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/walk"
)

type noSetupGlobFS struct{ fsys.FS }

func (noSetupGlobFS) Abs(string) (string, error) { panic("canceled glob touched filesystem") }

func TestGlobAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Glob(ctx, "/", Options{Pattern: "[", Walk: walk.Options{FS: noSetupGlobFS{}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

type cancelGlobFS struct {
	fsys.FS
	cancel context.CancelFunc
	closed atomic.Int64
}

func (f *cancelGlobFS) NewSession() fsys.Session {
	return &cancelGlobSession{Session: f.FS.NewSession(), f: f}
}

type cancelGlobSession struct {
	fsys.Session
	f *cancelGlobFS
}

func (s *cancelGlobSession) List(h fsys.Handle, detail fsys.Detail, children uint32) (*fsys.Listing, error) {
	ls, err := s.Session.List(h, detail, children)
	s.f.cancel()
	return ls, err
}

func (s *cancelGlobSession) Close(h fsys.Handle) {
	s.Session.Close(h)
	s.f.closed.Add(1)
}

func TestGlobNoResultsDoesNotHideCancellation(t *testing.T) {
	for _, ranked := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		f := &cancelGlobFS{FS: fsys.FromIOFS(fstest.MapFS{"a.txt": {}}), cancel: cancel}
		res, err := Glob(ctx, "/", Options{Pattern: "missing*", SortByMtime: ranked, Limit: 1, Walk: walk.Options{FS: f, Workers: 1}})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ranked=%v: got %v, want context.Canceled", ranked, err)
		}
		if res.Total != 0 || len(res.Matches) != 0 {
			t.Errorf("ranked=%v: canceled glob returned %+v", ranked, res)
		}
		if f.closed.Load() != 1 {
			t.Errorf("ranked=%v: closed %d root handles, want 1", ranked, f.closed.Load())
		}
	}
}
