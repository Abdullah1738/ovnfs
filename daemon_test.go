package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stencil-hq/ovnfs/api"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
)

// TestMain answers a spawned `serve` by exiting, so a startDaemon that spawns
// one fails instead of running the tests again in a child process.
var testServeFixture func()

func TestMain(m *testing.M) {
	if os.Getenv("OVNFS_TEST_RECEIPT_HOLDER") != "" && testServeFixture != nil {
		testServeFixture()
		os.Exit(0)
	}
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

// TestClientQuotesLogOfAFailedLoad drops a held hello, as a daemon whose state
// load fails does: a client that did not start it quotes the daemon log.
func TestClientQuotesLogOfAFailedLoad(t *testing.T) {
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	if err := os.WriteFile(logPath(state), []byte("read state.json: unexpected end of JSON input\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	go admin.NewServer().Serve(l)
	time.AfterFunc(200*time.Millisecond, func() { l.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := awaitDaemon(ctx, state, filepath.Join(state, "mnt"), 0, nil)
	if err == nil {
		client.Close()
		t.Fatal("hello answered by a daemon that never loaded")
	}
	if !strings.Contains(err.Error(), "read state.json: unexpected end of JSON input") {
		t.Fatalf("error without the daemon log: %v", err)
	}
}

type restartRefusal struct{}

func (restartRefusal) Call(_ context.Context, op string, _ json.RawMessage) (any, error) {
	if op == "status" {
		return api.Status{PID: os.Getpid()}, nil
	}
	if op == "stop" {
		return nil, &admin.RemoteError{Code: "test", Message: "planned stop reached"}
	}
	return nil, fmt.Errorf("unexpected operation %s", op)
}

func TestRestartStopsDaemonAfterDelayedHello(t *testing.T) {
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := admin.NewServer()
	go srv.Serve(l)
	time.AfterFunc(1200*time.Millisecond, func() {
		srv.Ready(admin.Welcome{Version: 1, Ops: []string{"status", "stop"}, Features: []string{"stop.keep_mount"}}, restartRefusal{}, nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &cli{ctx: ctx, state: state, mount: filepath.Join(state, "mnt")}
	defer c.close()
	if err := (&restartCmd{}).Run(c); err == nil || !strings.Contains(err.Error(), "planned stop reached") {
		t.Fatalf("restart skipped delayed daemon's planned stop: %v", err)
	}
}
