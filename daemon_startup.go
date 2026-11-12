package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/stencil-hq/ovnfs/admin"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type startupWaitKey struct{}

func (c *cli) startupContext() {
	if c.ctx == nil {
		c.ctx = context.Background()
	}
	if c.daemonTimeout > 0 {
		c.ctx = context.WithValue(c.ctx, startupWaitKey{}, c.daemonTimeout)
	}
}
func startupWait(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(startupWaitKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return serveLoadWait
}
func unresponsiveDaemon(state string, pid int) error {
	if pid > 0 {
		return fmt.Errorf("daemon %d holds %s but is not answering (wedged?)", pid, state)
	}
	return fmt.Errorf("daemon holds %s but is not answering (wedged? owner PID unavailable)", state)
}
func probeLoading(ctx context.Context, state string) (admin.Loading, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	return admin.Probe(ctx, socketPath(state))
}

// A missing lock is insufficient proof of absence: older daemons can retain
// their persisted TCP listener after an external disk loses the lock inode.
func checkSavedPort(state string) error {
	pid, held, err := savedPortHolder(state)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("daemon or process %d holds saved port for %s but admin.sock is not answering; refusing to start another daemon", pid, state)
	}
	return nil
}

func savedPortHolder(state string) (int, bool, error) {
	path := filepath.Join(state, "state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("can't read state: %s: %w", path, err)
	}
	var saved struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return 0, false, fmt.Errorf("can't read state: %s: %w", path, err)
	}
	if saved.Port == 0 {
		return 0, false, nil
	}
	if saved.Port < 0 || saved.Port > 65535 {
		return 0, false, fmt.Errorf("can't read state: %s: invalid saved port %d", path, saved.Port)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(saved.Port)))
	if err == nil {
		listener.Close()
		return 0, false, nil
	}
	// Binding can fail for reasons other than an existing holder. Refuse to
	// interpret any such error as permission to spawn against the persisted port.
	if !errors.Is(err, syscall.EADDRINUSE) {
		return 0, false, fmt.Errorf("checking saved port %d: %w", saved.Port, err)
	}
	return portHolder(saved.Port), true, nil
}

// Query only the TCP listener, never all descriptors (which can traverse a
// frozen NFS mount). Missing diagnostic tools cannot relax the port fence.
func portHolder(port int) int {
	if runtime.GOOS == "windows" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-a", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 1 {
		return 0
	}
	pid, _ := strconv.Atoi(fields[0])
	return pid
}

// A fresh diagnostic context can inspect the holder after the caller's wait
// budget expires; it cannot authorize process or mount changes.
func startupDeadline(state string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pid, held, err := daemonHolder(ctx, state)
	if err != nil {
		return fmt.Errorf("%w: %w", err, cause)
	}
	if !held {
		if p, ready, err := probeLoading(ctx, state); err == nil && !ready {
			pid = p.PID
			held = true
		}
	}
	if held {
		return fmt.Errorf("%w: %w", unresponsiveDaemon(state, pid), cause)
	}
	return fmt.Errorf("daemon at %s is not answering (wedged?): %w", state, cause)
}

// startupIdleContext preserves the standard deadline error while an idle timer
// can be extended by progress, unlike context.WithTimeout's fixed deadline.
type startupIdleContext struct{ context.Context }

func (c startupIdleContext) Err() error {
	if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

type startupIdleKey struct{}

func watchStartupProgress(parent context.Context, state string) (context.Context, context.CancelFunc) {
	if parent.Value(startupIdleKey{}) != nil {
		return parent, func() {}
	}
	limit := startupWait(parent)
	raw, cancel := context.WithCancelCause(parent)
	ctx := startupIdleContext{context.WithValue(raw, startupIdleKey{}, true)}
	go func() {
		timer := time.NewTimer(limit)
		defer timer.Stop()
		interval := min(time.Second, max(time.Millisecond, limit/4))
		tick := time.NewTicker(interval)
		defer tick.Stop()
		lastPID, lastN := 0, 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				cancel(context.DeadlineExceeded)
				return
			case <-tick.C:
				p, ready, err := probeLoading(ctx, state)
				if err != nil || ready {
					continue
				}
				advanced := p.PID == lastPID && p.Completed > lastN
				if p.PID != lastPID {
					advanced = p.Completed > 0
				}
				lastPID, lastN = p.PID, max(0, p.Completed)
				if advanced {
					timer.Reset(limit)
				}
			}
		}
	}()
	return ctx, func() { cancel(context.Canceled) }
}
