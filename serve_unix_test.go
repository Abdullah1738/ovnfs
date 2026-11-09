//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
)

// TestServeListensWhileLoading holds serve's state load on a named pipe: its
// admin socket accepts during the load and holds the client's hello, and a
// load that fails closes the held connection, so the client fails at once.
func TestServeListensWhileLoading(t *testing.T) {
	// A short path: a Unix socket path is limited to about 100 bytes.
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	stateFile := filepath.Join(state, "state.json")
	if err := syscall.Mkfifo(stateFile, 0o600); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- (&serveCmd{}).Run(&cli{state: state, mount: filepath.Join(state, "mnt")})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !accepting(state) {
		select {
		case err := <-served:
			t.Fatalf("serve returned before it listened: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no admin socket while the state loads")
		}
		time.Sleep(10 * time.Millisecond)
	}
	dialed := make(chan error, 1)
	go func() {
		client, err := admin.Dial(socketPath(state), adminProtocol)
		if err == nil {
			client.Close()
		}
		dialed <- err
	}()
	select {
	case err := <-dialed:
		t.Fatalf("hello answered while the state loads: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// Opening the pipe for writing releases serve's read of it.
	if err := os.WriteFile(stateFile, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err == nil || !strings.Contains(err.Error(), "state.json") {
			t.Fatalf("serve with an unreadable state: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve still loading after the state was written")
	}
	select {
	case err := <-dialed:
		if err == nil {
			t.Fatal("hello answered by a serve that failed its load")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client still waiting after serve failed its load")
	}
}
