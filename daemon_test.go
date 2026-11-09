package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
)

// TestMain answers a spawned `serve` by exiting, so a startDaemon that spawns
// one fails instead of running the tests again in a child process.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(3)
	}
	os.Exit(m.Run())
}

// TestStartWaitsForALoadingDaemon serves an admin socket that answers its
// hello only a second later, as a daemon loading its state does: startDaemon
// waits for that daemon and starts no serve of its own.
func TestStartWaitsForALoadingDaemon(t *testing.T) {
	// A short path: a Unix socket path is limited to about 100 bytes.
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	srv := admin.NewServer()
	go srv.Serve(l)
	time.AfterFunc(time.Second, func() { srv.Ready(admin.Welcome{Version: 1}, nil, nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, pid, err := startDaemon(ctx, state, filepath.Join(state, "mnt"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if pid != 0 {
		t.Fatalf("started serve pid %d, want none", pid)
	}
	if client.Welcome().Version != 1 {
		t.Fatal("missing welcome")
	}
}
