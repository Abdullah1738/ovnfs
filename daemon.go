package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
)

// daemonWait bounds how long stop waits for the socket to change state.
const daemonWait = 15 * time.Second

// serveLoadWait bounds how long a client waits for a daemon to answer its
// hello: a daemon answers once its state is loaded and mounted, which takes
// minutes on a large state after a reboot.
const serveLoadWait = 30 * time.Minute

// loadNotice is how long a client waits before it says it is still waiting.
const loadNotice = 5 * time.Second

// adminProtocol is the client name sent in the admin hello.
const adminProtocol = "ovnfs/1"

// startLock serializes daemon starts on one state directory.
const startLock = "start.lock"

// socketPath is the daemon's admin socket inside a state directory.
func socketPath(state string) string { return filepath.Join(state, "admin.sock") }

// nfsSocket is where a macOS daemon serves NFS to its mount (see mountLocal).
func nfsSocket(state string) string { return filepath.Join(state, "nfs.sock") }

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
	if c.autostart {
		client, pid, err := startDaemon(c.ctx, c.state, c.mount)
		if err != nil {
			return nil, err
		}
		if pid != 0 {
			fmt.Fprintf(stderr, "%s ovnfs (pid %d, log %s)\n", paint(stderr, styleGreen, "started"), pid, paint(stderr, styleDim, logPath(c.state)))
		}
		c.conn = client
		return client, nil
	}
	client, err := awaitDaemon(c.ctx, c.state, c.mount, 0, nil)
	if err != nil {
		return nil, err
	}
	c.conn = client
	return client, nil
}

// startDaemon spawns a detached `ovnfs serve` and waits until its admin socket
// completes a handshake. Starts on one state directory take turns; the pid is 0
// when an earlier start's daemon answers instead.
func startDaemon(ctx context.Context, state, mount string) (*admin.Client, int, error) {
	state, mount, err := daemonPaths(state, mount)
	if err != nil {
		return nil, 0, err
	}
	lock, err := lockDaemon(ctx, state)
	if err != nil {
		return nil, 0, err
	}
	defer lock.Close()
	return spawnDaemon(ctx, state, mount, lock)
}

// lockDaemon creates the state directory and waits for its start lock, held
// by startDaemon and restart while they change the daemon; closing the
// returned file releases it.
func lockDaemon(ctx context.Context, state string) (*os.File, error) {
	ctx, cancel := context.WithTimeout(ctx, serveLoadWait)
	defer cancel()
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, fmt.Errorf("creating state %s: %w", state, err)
	}
	lock, err := lockStart(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("waiting for other starts of %s: %w", state, err)
	}
	return lock, nil
}

// spawnDaemon is startDaemon for a caller that holds the start lock.
func spawnDaemon(ctx context.Context, state, mount string, lock *os.File) (*admin.Client, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	state, err := filepath.Abs(state)
	if err != nil {
		return nil, 0, fmt.Errorf("resolving state %s: %w", state, err)
	}
	if mount, err = filepath.Abs(mount); err != nil {
		return nil, 0, fmt.Errorf("resolving mount %s: %w", mount, err)
	}
	_, held, err := daemonHolder(ctx, state)
	if err != nil {
		return nil, 0, err
	}
	if accepting(state) || held {
		client, err := awaitDaemonLocked(ctx, state, mount, 0, nil)
		if errors.Is(err, errDaemonRecovered) {
			return spawnDaemon(ctx, state, mount, lock)
		}
		return client, 0, err
	}
	if err := finishRecovery(ctx, state, mount); err != nil {
		return nil, 0, err
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
	cmd := exec.Command(exe, "serve", "-state", state, "-mount", mount, "--starter-pid", strconv.Itoa(os.Getpid()))
	fd := inheritStart(cmd, lock)
	cmd.Args = append(cmd.Args, "--starter-fd", strconv.Itoa(fd))
	cmd.Stdin = nil // exec connects a nil Stdin to the null device
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = detachedProcess()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("starting ovnfs serve: %w", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); close(exited) }()
	client, err := awaitDaemonLocked(ctx, state, mount, pid, exited)
	if err != nil {
		failedStart(cmd, exited)
	}
	if errors.Is(err, errDaemonRecovered) {
		return spawnDaemon(ctx, state, mount, lock)
	}
	return client, pid, err
}

// awaitDaemon completes the admin hello on state's socket, waiting up to
// serveLoadWait while the daemon loads its state, and saying so on stderr
// after loadNotice. With exited nil the daemon is already listening, and a
// failed dial ends the wait. Otherwise the daemon is the serve this client
// started (pid): dials retry until its socket appears, and its exit ends the wait.
func awaitDaemon(ctx context.Context, state, mount string, pid int, exited <-chan error) (*admin.Client, error) {
	return awaitDaemonWait(ctx, state, mount, pid, exited, false)
}

func awaitDaemonLocked(ctx context.Context, state, mount string, pid int, exited <-chan error) (*admin.Client, error) {
	return awaitDaemonWait(ctx, state, mount, pid, exited, true)
}

func awaitDaemonWait(ctx context.Context, state, mount string, pid int, exited <-chan error, allowRecovery bool) (*admin.Client, error) {
	dialCtx, cancelDial := context.WithCancel(ctx)
	defer cancelDial()
	_, initiallyHeld, holderErr := daemonHolder(ctx, state)
	if holderErr != nil {
		return nil, holderErr
	}
	lastHealth := time.Time{}
	name := "ovnfs"
	if pid != 0 {
		name = fmt.Sprintf("ovnfs serve (pid %d)", pid)
	}
	type dialed struct {
		client *admin.Client
		err    error
	}
	var result chan dialed
	dial := func() {
		result = make(chan dialed, 1)
		go func(ch chan<- dialed) {
			client, err := admin.DialContext(dialCtx, socketPath(state), adminProtocol)
			ch <- dialed{client, err}
		}(result)
	}
	// abandon closes the connection of a dial still waiting for its welcome.
	abandon := func() {
		if result != nil {
			go func(ch <-chan dialed) {
				if r := <-ch; r.client != nil {
					r.client.Close()
				}
			}(result)
		}
	}
	deadline := time.NewTimer(serveLoadWait)
	defer deadline.Stop()
	notice := time.NewTimer(loadNotice)
	defer notice.Stop()
	retry := time.NewTicker(100 * time.Millisecond)
	defer retry.Stop()
	dial()
	for {
		select {
		case r := <-result:
			if r.err == nil {
				return r.client, nil
			}
			if exited == nil && !initiallyHeld {
				err := fmt.Errorf("connecting to ovnfs at %s: %w", socketPath(state), r.err)
				// Past the dial, the daemon accepted and then dropped the
				// hello: it failed while loading, and its log says why.
				var opErr *net.OpError
				if !errors.As(r.err, &opErr) || opErr.Op != "dial" {
					err = fmt.Errorf("%w; %s", err, logTail(state))
				}
				return nil, err
			}
			result = nil
		case <-retry.C:
			if time.Since(lastHealth) >= time.Second {
				lastHealth = time.Now()
				holder, held, err := daemonHolder(ctx, state)
				if err != nil {
					abandon()
					return nil, err
				}
				if allowRecovery && recoverySupported() && held && holder > 0 && checkout.Serving(state, holder) && daemonStopped(ctx, holder) {
					abandon()
					if err := recoverDaemon(ctx, state, mount, holder); err != nil {
						return nil, err
					}
					return nil, errDaemonRecovered
				}
				if initiallyHeld && !held && !accepting(state) && exited == nil {
					abandon()
					return nil, fmt.Errorf("daemon exited while loading; %s", logTail(state))
				}
			}
			if result == nil {
				dial()
			}
		case err := <-exited:
			abandon()
			if err == nil {
				err = errors.New("exit status 0")
			}
			return nil, fmt.Errorf("%s exited before accepting connections (%v); %s", name, err, logTail(state))
		case <-deadline.C:
			abandon()
			holder, held, err := daemonHolder(ctx, state)
			if err != nil {
				return nil, err
			}
			if allowRecovery && recoverySupported() && held && holder > 0 {
				if err := recoverDaemon(ctx, state, mount, holder); err != nil {
					return nil, err
				}
				return nil, errDaemonRecovered
			}
			return nil, fmt.Errorf("%s did not answer within %s; %s", name, serveLoadWait, logTail(state))
		case <-ctx.Done():
			abandon()
			return nil, fmt.Errorf("waiting for %s: %w", name, ctx.Err())
		case <-notice.C:
			fmt.Fprintf(stderr, "waiting for %s to load its state\n", name)
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

// startCmd makes sure the daemon runs, starting it in the background unless one
// already answers; --json prints its status (socket, mount, state) either way,
// which is how embedders such as omp find the socket to call.
type startCmd struct{}

func (cmd *startCmd) Run(c *cli) error {
	c.autostart = false
	client, pid, err := startDaemon(c.ctx, c.state, c.mount)
	if err != nil {
		return err
	}
	c.conn = client
	started := pid != 0
	st, err := call[api.Status](c, "status", struct{}{})
	if err != nil || c.json {
		return err
	}
	if started {
		fmt.Fprintf(stderr, "%s ovnfs (pid %d, log %s)\n", paint(stderr, styleGreen, "started"), st.PID, paint(stderr, styleDim, logPath(c.state)))
	} else {
		fmt.Fprintf(stderr, "%s (pid %d)\n", paint(stderr, styleDim, "already running"), st.PID)
	}
	return nil
}

// stopDaemon asks the running daemon to exit and waits until its process is
// gone, and with it the hold on the state directory the next daemon needs,
// returning the stop reply (nil when the daemon closed the connection first).
// It fails when the daemon refuses, as it does while its mount is busy, or
// still accepts connections after daemonWait. A daemon that stopped accepting
// is exiting: its final flush and state write can outlast daemonWait under
// load, so that wait has no bound.
func stopDaemon(c *cli, args api.StopArgs) (json.RawMessage, error) {
	raw, err := c.call("status", struct{}{})
	st, err := decodeReply[api.Status]("status", raw, err)
	if err != nil {
		return nil, err
	}
	raw, err = c.call("stop", args)
	if _, ok := errors.AsType[*admin.RemoteError](err); ok {
		return nil, err
	}
	// Any other error is the daemon closing the connection while it exits.
	c.close()
	c.conn = nil
	deadline := time.Now().Add(daemonWait)
	noticed := false
	for {
		up := accepting(c.state)
		if !up && (st.PID == 0 || !processAlive(st.PID)) {
			return raw, nil
		}
		if time.Now().After(deadline) {
			if up {
				return nil, fmt.Errorf("ovnfs still accepting connections at %s after %s", socketPath(c.state), daemonWait)
			}
			if !noticed {
				noticed = true
				fmt.Fprintf(stderr, "%s (pid %d)\n", paint(stderr, styleDim, "waiting for ovnfs to finish shutting down"), st.PID)
			}
		}
		select {
		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stopCmd asks the daemon to exit and waits until it has exited.
type stopCmd struct{}

func (cmd *stopCmd) Run(c *cli) error {
	c.autostart = false
	if !accepting(c.state) {
		fmt.Fprintln(stderr, paint(stderr, styleDim, "not running"))
		return nil
	}
	raw, err := stopDaemon(c, api.StopArgs{})
	if err != nil {
		return err
	}
	if c.json && raw != nil {
		fmt.Fprintln(stdout, string(raw))
	}
	fmt.Fprintln(stderr, paint(stderr, styleGreen, "stopped ovnfs"))
	return nil
}

// restartCmd replaces the running daemon with a fresh one spawned from this
// invocation's binary, which is how a rebuilt or upgraded ovnfs takes over. An
// NFS or SMB mount stays, so processes inside it carry on once the new daemon
// serves. It starts the daemon when none runs, and leaves the old one
// untouched if it refuses to stop. The start lock is held throughout, so
// clients that auto-start while the old daemon exits wait for the new one
// instead of racing it for the state directory.
type restartCmd struct{}

func (cmd *restartCmd) Run(c *cli) error {
	state, mount, err := daemonPaths(c.state, c.mount)
	if err != nil {
		return err
	}
	c.state, c.mount = state, mount
	c.autostart = false
	lock, err := lockDaemon(c.ctx, c.state)
	if err != nil {
		return err
	}
	defer lock.Close()
	_, held, err := daemonHolder(c.ctx, c.state)
	if err != nil {
		return err
	}
	if accepting(c.state) || held {
		client, err := awaitDaemonLocked(c.ctx, c.state, c.mount, 0, nil)
		if err != nil && !errors.Is(err, errDaemonRecovered) {
			return err
		}
		if client != nil {
			c.conn = client
			appendDaemonLog(c.state, fmt.Sprintf("restart requested by cli pid %d", os.Getpid()))
			// Daemons that predate the feature reject unknown fields.
			keep := slices.Contains(client.Welcome().Features, "stop.keep_mount")
			if _, err := stopDaemon(c, api.StopArgs{KeepMount: keep}); err != nil {
				return err
			}
		}
	}
	client, pid, err := spawnDaemon(c.ctx, c.state, c.mount, lock)
	if err != nil {
		return err
	}
	c.conn = client
	if c.json {
		_, err := call[api.Status](c, "status", struct{}{})
		return err
	}
	if pid == 0 {
		st, err := call[api.Status](c, "status", struct{}{})
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "already running (pid %d)\n", st.PID)
	} else {
		fmt.Fprintf(stderr, "%s ovnfs (pid %d, log %s)\n", paint(stderr, styleGreen, "restarted"), pid, paint(stderr, styleDim, logPath(c.state)))
	}
	return nil
}

// usage renders a memory tier's fill, or off when it has no limit.
func usage(u api.Usage, off string) string {
	if u.Limit == 0 {
		return off
	}
	return humanBytes(u.Used) + " of " + humanBytes(u.Limit)
}

// statusCmd prints the running daemon's identity and counts, or fails when none listens.
type statusCmd struct{}

func (cmd *statusCmd) Run(c *cli) error {
	c.autostart = false
	client, dialErr := dialDaemon(c.ctx, c.state)
	if dialErr != nil {
		pid, held, err := daemonHolder(c.ctx, c.state)
		if err != nil {
			return err
		}
		if held || accepting(c.state) {
			if pid > 0 {
				fmt.Fprintf(stderr, "ovnfs starting or unresponsive (pid %d, state lock held at %s)\n", pid, c.state)
			} else {
				fmt.Fprintf(stderr, "ovnfs starting or unresponsive at %s; owner PID unavailable\n", c.state)
			}
			return exitCode(1)
		}
		// Under --json stdout carries only JSON; the exit status says it all.
		out := io.Writer(stdout)
		if c.json {
			out = stderr
		}
		fmt.Fprintln(out, paint(out, styleRed, "not running"))
		return exitCode(1)
	}
	c.conn = client
	st, err := call[api.Status](c, "status", struct{}{})
	if err != nil || c.json {
		return err
	}
	transport := st.Proto + ":" + fmt.Sprint(st.Port) // protocol and file-server port, not a dialable address
	if st.Proto == "nfs" && st.Port == 0 {
		transport = st.Proto + ":" + nfsSocket(st.State)
	}
	rows := [][2]string{
		{"pid", fmt.Sprint(st.PID)},
		{"uptime", (time.Duration(st.Uptime * float64(time.Second))).Round(time.Second).String()},
		{"proto", transport},
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
		fmt.Fprintf(stdout, "%s%s%s\n", paint(stdout, styleDim, row[0]), strings.Repeat(" ", max(2, 12-len(row[0]))), row[1])
	}
	return nil
}

var errDaemonRecovered = errors.New("daemon holder recovered")

func dialDaemon(ctx context.Context, state string) (*admin.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return admin.DialContext(ctx, socketPath(state), adminProtocol)
}

func daemonPaths(state, mount string) (string, string, error) {
	state, err := filepath.Abs(state)
	if err != nil {
		return "", "", fmt.Errorf("state path: %w", err)
	}
	mount, err = filepath.Abs(mount)
	if err != nil {
		return "", "", fmt.Errorf("mount path: %w", err)
	}
	return state, mount, nil
}
