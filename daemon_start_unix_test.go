//go:build !windows

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestInheritedStartLeaseSurvivesParentCancellation(t *testing.T) {
	state := t.TempDir()
	lease, err := lockDaemon(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestInheritedStartLeaseProcess$")
	fd := inheritStart(cmd, lease)
	cmd.Env = append(os.Environ(), "OVNFS_TEST_START_STATE="+state, "OVNFS_TEST_START_FD="+strconv.Itoa(fd))
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		in.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("child: %v", err)
		}
	}()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "inherited\n" {
		t.Fatalf("child lease: %q %v", line, err)
	}
	// Cancellation closes the parent's copy; the child still owns the same lease.
	lease.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	other, err := lockDaemon(ctx, state)
	if err == nil {
		other.Close()
		t.Fatal("parent cancellation released child's exclusion")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestInheritedStartLeaseProcess(t *testing.T) {
	state := os.Getenv("OVNFS_TEST_START_STATE")
	if state == "" {
		return
	}
	fd, _ := strconv.Atoi(os.Getenv("OVNFS_TEST_START_FD"))
	lease, err := inheritedStartLease(fd, state)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	fmt.Println("inherited")
	io.Copy(io.Discard, os.Stdin)
}
