package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
)

// daemonWait bounds how long start and stop wait for the socket to change state.
const daemonWait = 15 * time.Second

// adminProtocol is the client name sent in the admin hello.
const adminProtocol = "ovnfs/1"

// startLock serializes daemon starts on one state directory.
const startLock = "start.lock"

// socketPath is the daemon's admin socket inside a state directory.
func socketPath(state string) string { return filepath.Join(state, "admin.sock") }

// logPath is where a background daemon's output is appended.
func logPath(state string) string { return filepath.Join(state, "serve.log") }

// accepting reports whether something listens on the admin socket, without a protocol handshake.
func accepting(state string) bool {
	conn, err := net.DialTimeout("unix", socketPath(state), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// client returns the daemon connection, auto-starting the daemon when the verb allows it.
func (c *cli) client() (*admin.Client, error) {
	if c.conn != nil {
		return c.conn, nil
	}
	if c.autostart && !accepting(c.state) {
		client, pid, err := startDaemon(c.ctx, c.state, c.mount)
		if err != nil {
			return nil, err
		}
		if pid != 0 {
			fmt.Fprintf(os.Stderr, "started ovnfs (pid %d, log %s)\n", pid, logPath(c.state))
		}
		c.conn = client
		return client, nil
	}
	client, err := admin.Dial(socketPath(c.state), adminProtocol)
	if err != nil {
		return nil, fmt.Errorf("connecting to ovnfs at %s: %w", socketPath(c.state), err)
	}
	c.conn = client
	return client, nil
}

// startDaemon spawns a detached `ovnfs serve` and waits until its admin socket
// completes a handshake. Starts on one state directory take turns; the pid is 0
// when an earlier start's daemon answers instead.
func startDaemon(ctx context.Context, state, mount string) (*admin.Client, int, error) {
	state, err := filepath.Abs(state)
	if err != nil {
		return nil, 0, fmt.Errorf("resolving state %s: %w", state, err)
	}
	if mount, err = filepath.Abs(mount); err != nil {
		return nil, 0, fmt.Errorf("resolving mount %s: %w", mount, err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, 0, fmt.Errorf("creating state %s: %w", state, err)
	}
	lock, err := lockStart(state)
	if err != nil {
		return nil, 0, fmt.Errorf("waiting for other starts of %s: %w", state, err)
	}
	defer lock.Close()
	if accepting(state) {
		client, err := admin.Dial(socketPath(state), adminProtocol)
		return client, 0, err
	}
	log, err := os.OpenFile(logPath(state), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("opening daemon log: %w", err)
	}
	defer log.Close()
	exe, err := os.Executable()
	if err != nil {
		return nil, 0, fmt.Errorf("locating ovnfs binary: %w", err)
	}
	cmd := exec.Command(exe, "serve", "-state", state, "-mount", mount)
	cmd.Stdin = nil // exec connects a nil Stdin to the null device
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = detachedProcess()
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("starting ovnfs serve: %w", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.NewTimer(daemonWait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if client, err := admin.Dial(socketPath(state), adminProtocol); err == nil {
			return client, pid, nil
		}
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("exit status 0")
			}
			return nil, pid, fmt.Errorf("ovnfs serve (pid %d) exited before accepting connections (%v); %s", pid, err, logTail(state))
		case <-deadline.C:
			return nil, pid, fmt.Errorf("ovnfs serve (pid %d) did not accept connections within %s; %s", pid, daemonWait, logTail(state))
		case <-ctx.Done():
			return nil, pid, fmt.Errorf("waiting for ovnfs serve (pid %d): %w", pid, ctx.Err())
		case <-tick.C:
		}
	}
}

// logTail quotes the last 20 lines of the daemon log for start failures.
func logTail(state string) string {
	lines, err := tailLines(logPath(state), 20)
	if err != nil {
		return fmt.Sprintf("reading %s: %v", logPath(state), err)
	}
	return fmt.Sprintf("last lines of %s:\n%s", logPath(state), strings.Join(lines, "\n"))
}

// tailLines returns up to n final lines of a file, reading at most its last 64 KiB.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const window = 64 << 10
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > window {
		if _, err := f.Seek(-window, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return lines[max(0, len(lines)-n):], nil
}

// runStart makes sure the daemon runs, starting it in the background unless one
// already answers; --json prints its status (socket, mount, state) either way,
// which is how embedders such as omp find the socket to call.
func runStart(c *cli, args []string) error {
	if _, err := c.parse(c.flags("start"), args, 0, 0); err != nil {
		return err
	}
	started := false
	if !accepting(c.state) {
		client, _, err := startDaemon(c.ctx, c.state, c.mount)
		if err != nil {
			return err
		}
		c.conn, started = client, true
	}
	st, err := call[api.Status](c, "status", struct{}{})
	if err != nil || c.json {
		return err
	}
	if started {
		fmt.Fprintf(os.Stderr, "started ovnfs (pid %d, log %s)\n", st.PID, logPath(c.state))
	} else {
		fmt.Fprintf(os.Stderr, "already running (pid %d)\n", st.PID)
	}
	return nil
}

// runStop asks the daemon to exit and waits until its socket stops accepting.
func runStop(c *cli, args []string) error {
	if _, err := c.parse(c.flags("stop"), args, 0, 0); err != nil {
		return err
	}
	if !accepting(c.state) {
		fmt.Fprintln(os.Stderr, "not running")
		return nil
	}
	raw, err := c.call("stop", struct{}{})
	if _, ok := errors.AsType[*admin.RemoteError](err); ok {
		return err
	}
	// Any other error is the daemon closing the connection while it exits.
	if err == nil && c.json {
		fmt.Println(string(raw))
	}
	c.close()
	c.conn = nil
	deadline := time.Now().Add(daemonWait)
	for accepting(c.state) {
		if time.Now().After(deadline) {
			return fmt.Errorf("ovnfs still accepting connections at %s after %s", socketPath(c.state), daemonWait)
		}
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	fmt.Fprintln(os.Stderr, "stopped ovnfs")
	return nil
}

// usage renders a memory tier's fill, or off when it has no limit.
func usage(u api.Usage, off string) string {
	if u.Limit == 0 {
		return off
	}
	return humanBytes(u.Used) + " of " + humanBytes(u.Limit)
}

// runStatus prints the running daemon's identity and counts, or fails when none listens.
func runStatus(c *cli, args []string) error {
	if _, err := c.parse(c.flags("status"), args, 0, 0); err != nil {
		return err
	}
	if !accepting(c.state) {
		fmt.Println("not running")
		return exitCode(1)
	}
	st, err := call[api.Status](c, "status", struct{}{})
	if err != nil || c.json {
		return err
	}
	rows := [][2]string{
		{"pid", fmt.Sprint(st.PID)},
		{"uptime", (time.Duration(st.Uptime * float64(time.Second))).Round(time.Second).String()},
		{"proto", st.Proto + ":" + fmt.Sprint(st.Port)}, // protocol and file-server port, not a dialable address
		{"mount", st.Mount},
		{"state", st.State},
		{"socket", st.Socket},
		{"checkouts", commas(int64(st.Checkouts))},
		{"snapshots", commas(int64(st.Snapshots))},
		{"bases", commas(int64(st.Bases))},
		{"layers", commas(int64(st.Layers))},
		{"memory", usage(st.Memory, "off: writes go to disk")},
		{"blob cache", usage(st.BlobCache, "off")},
	}
	for _, row := range rows {
		fmt.Printf("%-10s  %s\n", row[0], row[1])
	}
	return nil
}
