//go:build windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func init() {
	testServeFixture = func() {
		if os.Getenv("OVNFS_WINDOWS_FOREGROUND_FIXTURE") == "1" {
			state := os.Getenv("OVNFS_TEST_RECEIPT_HOLDER")
			fmt.Println("before start lease")
			lease, err := lockDaemon(context.Background(), state)
			if err != nil {
				os.Exit(4)
			}
			defer lease.Close()
			io.Copy(io.Discard, os.Stdin)
			return
		}
		if os.Getenv("OVNFS_WINDOWS_DETACHED_FIXTURE") == "1" {
			time.Sleep(time.Hour)
			return
		}
		fmt.Println("waiting")
		io.Copy(io.Discard, os.Stdin)
	}
}

func TestWindowsFailedStartupKeepsChildAndExclusion(t *testing.T) {
	state := startupState(t)
	lease, err := lockDaemon(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "serve", "--state", state, "--starter-pid", strconv.Itoa(os.Getpid()), "--starter-fd=0")
	child.Env = append(os.Environ(), "OVNFS_TEST_RECEIPT_HOLDER="+state)
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "waiting\n" {
		t.Fatalf("fixture %q %v", line, err)
	}
	releaseStartupLease(lease, state, child.Process.Pid, context.DeadlineExceeded)
	if !processAlive(child.Process.Pid) {
		t.Fatal("loading child killed on timeout")
	}
	// No start lease, admin socket or registry lock remains. Discovery must
	// still find the loading child independently of its starter's lifetime.
	pid, held, err := daemonHolder(context.Background(), state)
	if err != nil || !held || pid != child.Process.Pid {
		t.Fatalf("lost pending child: pid%d held%t err%v", pid, held, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	c, spawned, err := startDaemon(ctx, state, state+"-mount")
	if c != nil {
		c.Close()
	}
	if err == nil || spawned != 0 {
		t.Fatalf("spawned while child alive: pid%d err%v", spawned, err)
	}
	if !processAlive(child.Process.Pid) {
		t.Fatal("timeout killed child")
	}
}

func TestWindowsPendingServeSurvivesActualStarterExit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if state := os.Getenv("OVNFS_WINDOWS_EXITING_STARTER"); state != "" {
		child := exec.Command(exe, "serve", "--state", state, "--starter-pid", strconv.Itoa(os.Getpid()), "--starter-fd=0")
		child.Env = append(os.Environ(), "OVNFS_TEST_RECEIPT_HOLDER="+state, "OVNFS_WINDOWS_DETACHED_FIXTURE=1")
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		fmt.Println(child.Process.Pid)
		os.Exit(0)
	}
	state := startupState(t)
	starter := exec.Command(exe, "-test.run=^TestWindowsPendingServeSurvivesActualStarterExit$")
	starter.Env = append(os.Environ(), "OVNFS_WINDOWS_EXITING_STARTER="+state)
	out, err := starter.CombinedOutput()
	if err != nil {
		t.Fatalf("starter: %s %v", out, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("child pid: %q %v", out, err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()

	for label, alias := range stateDirectoryAliases(t, state) {
		t.Run(label, func(t *testing.T) {
			holder, held, err := daemonHolder(context.Background(), alias)
			if err != nil || !held || holder != pid {
				t.Fatalf("starter exited, pending child lost via %q: pid%d held%t err%v", alias, holder, held, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			c, spawned, err := startDaemon(ctx, alias, state+"-mount")
			if c != nil {
				c.Close()
			}
			if err == nil || spawned != 0 || !processAlive(pid) {
				t.Fatalf("duplicate startup via %q after parent exit: pid%d err%v", alias, spawned, err)
			}
		})
	}
}

func TestWindowsPendingDiscoverySkipsForegroundLeaseWaiter(t *testing.T) {
	state := startupState(t)
	lease, err := lockDaemon(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "serve", "--state", state)
	child.Env = append(os.Environ(), "OVNFS_TEST_RECEIPT_HOLDER="+state, "OVNFS_WINDOWS_FOREGROUND_FIXTURE=1")
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "before start lease\n" {
		t.Fatalf("foreground fixture %q %v", line, err)
	}
	// This CLI owns the lease, while the foreground serve is waiting for it.
	// Neither pending discovery nor the no-spawn gate may treat it as a holder.
	if pid, held, err := pendingStartupHolder(context.Background(), state); err != nil || held {
		t.Fatalf("foreground waiter classified as pending holder: pid%d held%t err%v", pid, held, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, pid, err := spawnDaemon(ctx, state, state+"-mount", lease)
	if c != nil {
		c.Close()
	}
	if err == nil || pid == 0 {
		t.Fatalf("starter failed to reach its own replacement: pid%d err%v", pid, err)
	}
}
