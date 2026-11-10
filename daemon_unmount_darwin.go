//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/stencil-hq/ovnfs/checkout"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// A timed-out umount may remain in kernel I/O wait after SIGKILL. Persist its
// identity so a later start cannot mount a new server underneath that unmount.
type unmountReceipt struct {
	BootSec   int64  `json:"boot_sec"`
	Phase     string `json:"phase"`
	Mount     string `json:"mount"`
	PID       int    `json:"pid"`
	BirthSec  int64  `json:"birth_sec"`
	BirthUsec int32  `json:"birth_usec"`
}

func saveUnmount(state string, r unmountReceipt) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := filepath.Join(state, "unmount.json")
	if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func lockUnmount(ctx context.Context, state string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(state, "unmount.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func finishRecovery(ctx context.Context, state, mount string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := lockUnmount(ctx, state)
	if err != nil {
		return err
	}
	defer lease.Close()
	return finishRecoveryLocked(ctx, state, mount)
}

func finishRecoveryLocked(ctx context.Context, state, mount string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(state, "unmount.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var r unmountReceipt
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("read recovery receipt: %w", err)
	}
	if r.Phase == "done" {
		return nil
	}
	if r.Mount != mount {
		return fmt.Errorf("pending recovery belongs to mount %s, not %s", r.Mount, mount)
	}
	if boot, err := unix.SysctlKinfoProc("kern.proc.pid", 1); err == nil && r.BootSec > 0 && boot.Proc.P_starttime.Sec > 0 && boot.Proc.P_starttime.Sec != r.BootSec {
		r.Phase = "failed"
		r.PID = 0
	}
	switch r.Phase {
	case "starting":
		return fmt.Errorf("unmount launch was interrupted before its pid was recorded; reconcile %s before starting", filepath.Join(state, "unmount.json"))
	case "running":
		if r.PID <= 0 {
			return fmt.Errorf("invalid pending unmount pid %d", r.PID)
		}
		if processAlive(r.PID) {
			p, err := unix.SysctlKinfoProc("kern.proc.pid", r.PID)
			if err != nil || r.BirthSec == 0 || p.Proc.P_starttime.Sec == r.BirthSec && p.Proc.P_starttime.Usec == r.BirthUsec {
				return fmt.Errorf("previous unmount pid %d has not exited; refusing to start another daemon", r.PID)
			}
		}
	case "failed":
	default:
		return fmt.Errorf("unknown recovery receipt phase %q", r.Phase)
	}
	if _, held, err := checkout.StateHolder(state); err != nil {
		return err
	} else if held {
		r.Phase = "done"
		return saveUnmount(state, r)
	}
	// The recorded process is gone (or the pid has been reused), so no old
	// unmount can remove a replacement's mount later. Retry only our own mount.
	if !mounted(mount) {
		r.Phase = "done"
		return saveUnmount(state, r)
	}
	return detachStaleMountLocked(ctx, state, mount)
}

func trackedUnmount(ctx context.Context, state, mount string, force bool) error {
	lease, err := lockUnmount(ctx, state)
	if err != nil {
		return err
	}
	defer lease.Close()
	return trackedUnmountLocked(ctx, state, mount, force)
}

func trackedUnmountLocked(ctx context.Context, state, mount string, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	boot, err := unix.SysctlKinfoProc("kern.proc.pid", 1)
	if err != nil {
		return err
	}
	if boot.Proc.P_starttime.Sec <= 0 {
		return fmt.Errorf("cannot identify boot for unmount recovery")
	}
	r := unmountReceipt{Phase: "starting", Mount: mount, BootSec: boot.Proc.P_starttime.Sec}
	if err := saveUnmount(state, r); err != nil {
		return err
	}
	args := []string{mount}
	if force {
		args = append([]string{"-f"}, args...)
	}
	cmd := exec.Command("/sbin/umount", args...)
	if err := ctx.Err(); err != nil {
		r.Phase = "failed"
		_ = saveUnmount(state, r)
		return err
	}
	if err := cmd.Start(); err != nil {
		r.Phase = "failed"
		_ = saveUnmount(state, r)
		return err
	}
	r.Phase = "running"
	r.PID = cmd.Process.Pid
	if p, err := unix.SysctlKinfoProc("kern.proc.pid", r.PID); err == nil {
		r.BirthSec = p.Proc.P_starttime.Sec
		r.BirthUsec = p.Proc.P_starttime.Usec
	}
	if err := saveUnmount(state, r); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			r.Phase = "done"
		} else {
			r.Phase = "failed"
		}
		if saveErr := saveUnmount(state, r); saveErr != nil {
			return saveErr
		}
		return err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		// Do not wait for an uninterruptible child. Its receipt remains active
		// until a later start confirms that this exact process has exited.
		return ctx.Err()
	}
}

// Shutdown can outlive its daemon's request context. Its helper needs the
// same durable fence as recovery if it remains blocked after escalation.
func releaseMount(state, mount string, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := lockUnmount(ctx, state)
	if err != nil {
		return err
	}
	defer lease.Close()
	if err := finishRecoveryLocked(ctx, state, mount); err != nil {
		return err
	}
	if !mounted(mount) {
		return nil
	}
	return trackedUnmountLocked(ctx, state, mount, force)
}
