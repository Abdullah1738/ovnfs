//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPendingUnmountPreventsAnotherStart(t *testing.T) {
	state := t.TempDir()
	mount := filepath.Join(state, "mount")
	p, err := unix.SysctlKinfoProc("kern.proc.pid", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	r := unmountReceipt{Phase: "running", Mount: mount, PID: os.Getpid(), BirthSec: p.Proc.P_starttime.Sec, BirthUsec: p.Proc.P_starttime.Usec}
	if err := saveUnmount(state, r); err != nil {
		t.Fatal(err)
	}
	if err := finishRecovery(context.Background(), state, mount); err == nil {
		t.Fatal("pending unmount allowed another start")
	}
}

func TestUnmountReceiptReconciliation(t *testing.T) {
	for _, row := range []struct {
		phase     string
		pid       int
		birth     int64
		wantError bool
	}{
		{"starting", 0, 0, true},
		{"running", 0, 0, true},
		{"running", -1, 0, true},
		{"running", os.Getpid(), 1, false},
		{"failed", 0, 0, false},
		{"done", os.Getpid(), 0, false},
		{"unknown", 0, 0, true},
	} {
		t.Run(fmt.Sprintf("%s/pid=%d", row.phase, row.pid), func(t *testing.T) {
			state := t.TempDir()
			mount := filepath.Join(state, "mount")
			r := unmountReceipt{Phase: row.phase, Mount: mount, PID: row.pid, BirthSec: row.birth}
			if err := saveUnmount(state, r); err != nil {
				t.Fatal(err)
			}
			if err := finishRecovery(context.Background(), state, mount); (err != nil) != row.wantError {
				t.Fatalf("receipt: %v wantError=%t", err, row.wantError)
			}
		})
	}
}

func TestUnmountFromPreviousBootDoesNotBlockStartup(t *testing.T) {
	state := t.TempDir()
	mount := filepath.Join(state, "mount")
	if err := saveUnmount(state, unmountReceipt{Phase: "starting", Mount: mount, BootSec: 1}); err != nil {
		t.Fatal(err)
	}
	if err := finishRecovery(context.Background(), state, mount); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledUnmountHasNoLaunchReceipt(t *testing.T) {
	state := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := trackedUnmount(ctx, state, filepath.Join(state, "mount"), true); err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, err := os.Stat(filepath.Join(state, "unmount.json")); !os.IsNotExist(err) {
		t.Fatalf("canceled unmount wrote receipt: %v", err)
	}
}

func TestForegroundServeRejectsPendingUnmount(t *testing.T) {
	for _, starter := range []int{0, os.Getppid()} {
		t.Run(fmt.Sprint(starter), func(t *testing.T) {
			state := t.TempDir()
			mount := filepath.Join(state, "mount")
			if err := saveUnmount(state, unmountReceipt{Phase: "starting", Mount: mount}); err != nil {
				t.Fatal(err)
			}
			c := &cli{ctx: context.Background(), state: state, mount: mount}
			if err := (&serveCmd{StarterPID: starter}).Run(c); err == nil || !strings.Contains(err.Error(), "unmount launch was interrupted") {
				t.Fatalf("foreground serve bypassed receipt: %v", err)
			}
			if _, err := os.Stat(socketPath(state)); !os.IsNotExist(err) {
				t.Fatalf("foreground serve claimed admin socket before fence: %v", err)
			}
		})
	}
}

func TestShutdownDoesNotLaunchOverPendingUnmount(t *testing.T) {
	for _, force := range []bool{false, true} {
		state := t.TempDir()
		mount := filepath.Join(state, "mount")
		if err := saveUnmount(state, unmountReceipt{Phase: "starting", Mount: mount}); err != nil {
			t.Fatal(err)
		}
		if err := releaseMount(state, mount, force); err == nil || !strings.Contains(err.Error(), "unmount launch was interrupted") {
			t.Fatalf("shutdown bypassed receipt: %v", err)
		}
	}
}

func TestUnmountReceiptUpdatesShareExclusion(t *testing.T) {
	state := t.TempDir()
	mount := filepath.Join(state, "mount")
	lease, err := lockUnmount(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	// A second stop replaces a failed attempt while holding the receipt lease.
	if err := saveUnmount(state, unmountReceipt{Phase: "failed", Mount: mount}); err != nil {
		t.Fatal(err)
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	active := unmountReceipt{Phase: "running", Mount: mount, PID: os.Getpid(), BirthSec: p.Proc.P_starttime.Sec, BirthUsec: p.Proc.P_starttime.Usec}
	if err := saveUnmount(state, active); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := finishRecovery(ctx, state, mount); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconciliation bypassed active transition: %v", err)
	}
	lease.Close()
	if err := finishRecovery(context.Background(), state, mount); err == nil {
		t.Fatal("active helper fence erased")
	}
	data, err := os.ReadFile(filepath.Join(state, "unmount.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got unmountReceipt
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != active {
		t.Fatalf("receipt changed: %+v", got)
	}
}

func TestLiveHolderReconcilesCompletedUnmount(t *testing.T) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, phase string
		pid         int
		birth       int64
		wantError   bool
	}{
		{"failed-before-launch", "failed", 0, 0, false},
		{"failed-completed-helper", "failed", os.Getpid(), p.Proc.P_starttime.Sec, false},
		{"running-reused-pid", "running", os.Getpid(), 1, false},
		{"running-active-helper", "running", os.Getpid(), p.Proc.P_starttime.Sec, true},
		{"running-unverified-birth", "running", os.Getpid(), 0, true},
		{"starting-unknown-helper", "starting", 0, 0, true},
		{"done", "done", os.Getpid(), 0, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			state := t.TempDir()
			mount := filepath.Join(state, "mount")
			reg, err := checkout.Open(checkout.Config{State: state, Mount: mount})
			if err != nil {
				t.Fatal(err)
			}
			defer reg.Close()
			receipt := unmountReceipt{Phase: row.phase, Mount: mount, PID: row.pid, BirthSec: row.birth, BirthUsec: p.Proc.P_starttime.Usec}
			if err := saveUnmount(state, receipt); err != nil {
				t.Fatal(err)
			}
			if err := finishRecovery(context.Background(), state, mount); (err != nil) != row.wantError {
				t.Fatalf("reconcile: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(state, "unmount.json"))
			if err != nil {
				t.Fatal(err)
			}
			var got unmountReceipt
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if row.wantError && got != receipt {
				t.Fatalf("active helper fence changed: %+v", got)
			}
			if !row.wantError && got.Phase != "done" {
				t.Fatalf("completed receipt not reconciled: %+v", got)
			}
		})
	}
}

type legacyHandoffStopper struct {
	state       string
	interleaved chan error
}

func (h legacyHandoffStopper) Call(_ context.Context, op string, _ json.RawMessage) (any, error) {
	// The status call occurs after client reconciliation and before keep_mount.
	// A competing stop must not be able to record another failed unmount here.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	lease, err := lockUnmount(ctx, h.state)
	if err == nil {
		_ = saveUnmount(h.state, unmountReceipt{Phase: "failed", Mount: filepath.Join(h.state, "mount")})
		lease.Close()
		h.interleaved <- fmt.Errorf("concurrent stop interleaved at %s", op)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		h.interleaved <- err
	}
	if op == "status" {
		return api.Status{PID: os.Getpid()}, nil
	}
	return nil, &admin.RemoteError{Code: "test", Message: "planned stop reached"}
}

func TestLegacyHandoffExcludesConcurrentFailedStop(t *testing.T) {
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(state)
	mount := filepath.Join(state, "mount")
	reg, err := checkout.Open(checkout.Config{State: state, Mount: mount})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if err := saveUnmount(state, unmountReceipt{Phase: "failed", Mount: mount}); err != nil {
		t.Fatal(err)
	}
	listener, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	handler := legacyHandoffStopper{state: state, interleaved: make(chan error, 2)}
	go admin.Serve(listener, admin.Welcome{Version: 1, Ops: []string{"status", "stop"}, Features: []string{"stop.keep_mount"}}, handler, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &cli{ctx: ctx, state: state, mount: mount}
	defer c.close()
	if err := (&restartCmd{}).Run(c); err == nil || !strings.Contains(err.Error(), "planned stop reached") {
		t.Fatalf("handoff: %v", err)
	}
	select {
	case err := <-handler.interleaved:
		t.Fatal(err)
	default:
	}
	lease, err := lockUnmount(ctx, state)
	if err != nil {
		t.Fatalf("handoff leaked receipt lock: %v", err)
	}
	lease.Close()
}

func TestHandoffRefusesHolderLossDuringReconciliation(t *testing.T) {
	state := t.TempDir()
	mount := filepath.Join(state, "mount")
	reg, err := checkout.Open(checkout.Config{State: state, Mount: mount})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := lockUnmount(context.Background(), state)
	if err != nil {
		reg.Close()
		t.Fatal(err)
	}
	defer lease.Close()
	if _, held, err := checkout.StateHolder(state); err != nil || !held {
		reg.Close()
		t.Fatalf("initial holder: %t %v", held, err)
	}
	// Legacy TERM ignores unmount.lock: the holder can disappear after the
	// first check. Handoff reconciliation must never fall back to detachment.
	reg.Close()
	for _, phase := range []string{"failed", "running"} {
		r := unmountReceipt{Phase: phase, Mount: mount, PID: os.Getpid(), BirthSec: 1}
		if err := saveUnmount(state, r); err != nil {
			t.Fatal(err)
		}
		if err := finishHandoffLocked(context.Background(), state, mount); err == nil || !strings.Contains(err.Error(), "daemon exited") {
			t.Fatalf("holder loss: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(state, "unmount.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got unmountReceipt
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got != r {
			t.Fatalf("receipt changed after holder loss: %+v", got)
		}
	}
}
