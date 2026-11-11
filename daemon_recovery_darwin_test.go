//go:build darwin

package main

import (
	"bufio"
	"context"
	"fmt"
	"github.com/stencil-hq/ovnfs/checkout"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	testServeFixture = func() {
		state := os.Getenv("OVNFS_TEST_RECEIPT_HOLDER")
		reg, err := checkout.Open(checkout.Config{State: state, Mount: filepath.Join(state, "mount")})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		defer reg.Close()
		if err := checkout.MarkServing(state); err != nil {
			os.Exit(4)
		}
		lease, err := lockUnmount(context.Background(), state)
		if err != nil {
			os.Exit(4)
		}
		defer lease.Close()
		fmt.Println("locked")
		syscall.Kill(os.Getpid(), syscall.SIGSTOP)
		select {}
	}
}

func TestStoppedHolderWithUnmountLeaseIsNotReplaced(t *testing.T) {
	state := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(exe, "serve", "--state", state)
	holder.Env = append(os.Environ(), "OVNFS_TEST_RECEIPT_HOLDER="+state)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- holder.Wait(); close(exited) }()
	defer func() { holder.Process.Kill(); <-exited }()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("holder: %q %v", line, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	client, _, err := startDaemon(ctx, state, filepath.Join(state, "mount"))
	if client != nil {
		client.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "wedged?") {
		t.Fatalf("missing bounded wait diagnostic: %v", err)
	}
	select {
	case <-exited:
		t.Fatal("startup killed a live holder")
	default:
	}
	if !processAlive(holder.Process.Pid) {
		t.Fatal("holder died")
	}
}
