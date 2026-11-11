package admin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadingProbeBeforeReady(t *testing.T) {
	dir, err := os.MkdirTemp("", "ovnfs-probe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "admin.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := NewServer()
	s.Loading(42, 3, 10)
	go s.Serve(l)
	p, ready, err := Probe(context.Background(), path)
	if err != nil || ready || p != (Loading{42, 3, 10}) {
		t.Fatalf("%+v %t %v", p, ready, err)
	}
	s.Loading(42, 10, 10)
	s.Ready(Welcome{Version: 1}, nil, nil)
	p, ready, err = Probe(context.Background(), path)
	if err != nil || !ready || p.Completed != 10 {
		t.Fatalf("%+v %t %v", p, ready, err)
	}
	c, err := Dial(path, "old-client")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
