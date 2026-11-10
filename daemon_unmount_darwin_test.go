//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
