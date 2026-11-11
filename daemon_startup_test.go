package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
)

func startupState(t *testing.T) string {
	t.Helper()
	p, err := os.MkdirTemp("", "ovnfs700-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0700); os.RemoveAll(p) })
	return p
}

func TestStartupHeldLockTimeoutNeverSpawns(t *testing.T) {
	state := startupState(t)
	reg, err := checkout.Open(checkout.Config{State: state, Mount: filepath.Join(state, "mount")})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx := context.WithValue(context.Background(), startupWaitKey{}, 40*time.Millisecond)
	client, pid, err := startDaemon(ctx, state, filepath.Join(state, "mount"))
	if client != nil {
		client.Close()
	}
	if err == nil || pid != 0 || !strings.Contains(err.Error(), "wedged?") {
		t.Fatalf("pid=%d err=%v", pid, err)
	}
	if _, err := os.Stat(logPath(state)); !os.IsNotExist(err) {
		t.Fatalf("spawned a serve: %v", err)
	}
	if _, held, err := checkout.StateHolder(state); err != nil || !held {
		t.Fatalf("holder lost: %t %v", held, err)
	}
}

func TestStartupConcurrentHeldDaemonWaits(t *testing.T) {
	state := startupState(t)
	reg, err := checkout.Open(checkout.Config{State: state, Mount: filepath.Join(state, "mount")})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	// Queue callers behind an existing starter, then publish the one daemon.
	lease, err := lockDaemon(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	const count = 8
	var wg sync.WaitGroup
	failures := make(chan error, count)
	for range count {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, pid, err := startDaemon(ctx, state, filepath.Join(state, "mount"))
			if c != nil {
				c.Close()
			}
			if err != nil || pid != 0 {
				failures <- fmt.Errorf("pid=%d err=%v", pid, err)
			}
		})
	}
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go admin.Serve(l, admin.Welcome{Version: 1}, nil, nil)
	lease.Close()
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if _, err := os.Stat(logPath(state)); !os.IsNotExist(err) {
		t.Fatalf("spawned a serve: %v", err)
	}
}

func TestStartupSavedPortAndUnreadableState(t *testing.T) {
	state := startupState(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	path := filepath.Join(state, "state.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"port":%d}`, l.Addr().(*net.TCPAddr).Port)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0600, 0} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, readErr := os.ReadFile(path)
		for _, verb := range []string{"start", "ls", "status"} {
			c := &cli{ctx: context.Background(), state: state, mount: filepath.Join(state, "mount"), autostart: true, daemonTimeout: 50 * time.Millisecond}
			var err error
			switch verb {
			case "start":
				err = (&startCmd{}).Run(c)
			case "ls":
				err = (&lsCmd{}).Run(c)
			case "status":
				err = (&statusCmd{}).Run(c)
			}
			c.close()
			if err == nil {
				t.Fatalf("%s mode %o ignored port/state", verb, mode)
			}
			if mode == 0 && os.IsPermission(readErr) && !strings.Contains(err.Error(), "can't read state:") {
				t.Fatalf("%s unreadable: %v", verb, err)
			}
		}
	}
	os.Chmod(path, 0600)
	if _, err := os.Stat(logPath(state)); !os.IsNotExist(err) {
		t.Fatalf("spawned a serve: %v", err)
	}
}

type startupHandler struct{}

func (startupHandler) Call(_ context.Context, op string, _ json.RawMessage) (any, error) {
	if op == "ls" {
		return api.LsReply{}, nil
	}
	return nil, fmt.Errorf("unexpected %s", op)
}

func TestStartupHealthyReadOnlyState(t *testing.T) {
	state := startupState(t)
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go admin.Serve(l, admin.Welcome{Version: 1, Ops: []string{"ls"}}, startupHandler{}, nil)
	lease, err := lockDaemon(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	os.Chmod(state, 0500)
	defer os.Chmod(state, 0700)
	t.Setenv("OVNFS_STATE", state)
	t.Setenv("OVNFS_MOUNT", filepath.Join(state, "mount"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	c := &cli{ctx: context.Background(), state: state, mount: filepath.Join(state, "mount"), autostart: true, daemonTimeout: 50 * time.Millisecond}
	defer c.close()
	if err := (&lsCmd{}).Run(c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath(state)); !os.IsNotExist(err) {
		t.Fatalf("healthy daemon spawned: %v", err)
	}
}

func TestStartupLoadingStatus(t *testing.T) {
	state := startupState(t)
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := admin.NewServer()
	srv.Loading(123, 2, 9)
	go srv.Serve(l)
	var out strings.Builder
	before := stdout
	stdout = &colorprofile.Writer{Forward: &out}
	defer func() { stdout = before }()
	c := &cli{ctx: context.Background(), state: state, json: true}
	if err := (&statusCmd{}).Run(c); err == nil {
		t.Fatal("loading status claimed ready")
	}
	if text := out.String(); !strings.Contains(text, `"pid":123`) || !strings.Contains(text, `"completed":2`) {
		t.Fatalf("status=%s", text)
	}
}

func TestStartupTimeoutFlagForms(t *testing.T) {
	for _, args := range [][]string{{"--daemon-timeout", "2s", "ls"}, {"ls", "-daemon-timeout=2s"}, {"--daemon-timeout=2s", "start"}} {
		c := &cli{}
		if _, err := splitGlobals(c, args); err != nil || c.daemonTimeout != 2*time.Second {
			t.Fatalf("%v: %v %s", args, err, c.daemonTimeout)
		}
	}
	for _, value := range []string{"0", "-1s", "bad"} {
		if _, err := splitGlobals(&cli{}, []string{"ls", "--daemon-timeout=" + value}); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}

func TestStartupLoadingSocketTimeoutNamesState(t *testing.T) {
	state := startupState(t)
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := admin.NewServer()
	srv.Loading(31415, 0, 8)
	go srv.Serve(l)
	ctx := context.WithValue(context.Background(), startupWaitKey{}, 30*time.Millisecond)
	c, pid, err := startDaemon(ctx, state, filepath.Join(state, "mount"))
	if c != nil {
		c.Close()
	}
	if err == nil || pid != 0 || !strings.Contains(err.Error(), state) || !strings.Contains(err.Error(), "wedged?") {
		t.Fatalf("pid=%d err=%v", pid, err)
	}
}

func TestStartupHealthyDaemonRespectsRestartLease(t *testing.T) {
	state := startupState(t)
	lease, err := lockDaemon(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go admin.Serve(l, admin.Welcome{Version: 111}, nil, nil)
	ctx := context.WithValue(context.Background(), startupWaitKey{}, 40*time.Millisecond)
	c, pid, err := startDaemon(ctx, state, filepath.Join(state, "mount"))
	if c != nil {
		c.Close()
	}
	if err == nil || pid != 0 {
		t.Fatalf("connected to retiring daemon while restart held lease: pid%d err%v", pid, err)
	}
	lease.Close()
	c, pid, err = startDaemon(context.Background(), state, filepath.Join(state, "mount"))
	if err != nil || pid != 0 {
		t.Fatalf("healthy released: pid%d err%v", pid, err)
	}
	c.Close()
}

func TestStartupProgressExtendsIdleBudget(t *testing.T) {
	state := startupState(t)
	lease, err := lockDaemon(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := admin.NewServer()
	srv.Loading(100, 0, 6)
	go srv.Serve(l)
	// Keep the caller on start.lock for more than one idle budget, as a second
	// client of an active cold start would be. Progress, not elapsed wall time,
	// authorizes the continued wait.
	for n := 1; n <= 6; n++ {
		n := n
		time.AfterFunc(time.Duration(n)*60*time.Millisecond, func() {
			srv.Loading(100, n, 6)
			if n == 6 {
				srv.Ready(admin.Welcome{Version: 1}, nil, nil)
				lease.Close()
			}
		})
	}
	ctx := context.WithValue(context.Background(), startupWaitKey{}, 200*time.Millisecond)
	started := time.Now()
	c, pid, err := startDaemon(ctx, state, filepath.Join(state, "mount"))
	if c != nil {
		c.Close()
	}
	if err != nil || pid != 0 {
		t.Fatalf("progress timed out: pid%d err%v", pid, err)
	}
	if time.Since(started) < 200*time.Millisecond {
		t.Fatal("fixture did not exceed initial idle budget")
	}
}

func TestStartupDefaultIdleBudget(t *testing.T) {
	if startupWait(context.Background()) != 5*time.Minute {
		t.Fatal("default idle wait must be five minutes")
	}
}
