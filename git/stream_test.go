package git

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestObjectSizes answers every question in order: a missing object fails only
// its own question, and a killed git fails the questions it left unanswered.
func TestObjectSizes(t *testing.T) {
	r := testRepo(t)
	blob := testGit(t, r.Top, "rev-parse", "HEAD:tracked")
	type answer struct {
		size int64
		err  error
	}
	got := map[string]answer{}
	s, err := ObjectSizes(context.Background(), r, func(tag string, size int64, err error) { got[tag] = answer{size, err} })
	if err != nil {
		t.Fatal(err)
	}
	s.Add(blob, "first")
	s.Add(strings.Repeat("0", len(blob)), "missing")
	s.Add(blob, "after")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if a := got["first"]; a.size != int64(len("original\n")) || a.err != nil {
		t.Fatalf("first = %+v", a)
	}
	if a := got["missing"]; !errors.Is(a.err, os.ErrNotExist) {
		t.Fatalf("missing = %+v", a)
	}
	if a := got["after"]; a.size != int64(len("original\n")) || a.err != nil {
		t.Fatalf("after missing = %+v", a)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if s, err = ObjectSizes(ctx, r, func(tag string, size int64, err error) { got[tag] = answer{size, err} }); err != nil {
		t.Fatal(err)
	}
	cancel()
	s.Add(blob, "killed")
	if err := s.Close(); err == nil {
		t.Fatal("killed stream closed cleanly")
	}
	if a, ok := got["killed"]; !ok || a.err == nil {
		t.Fatalf("question to a killed git = %+v, answered %v", a, ok)
	}
}
