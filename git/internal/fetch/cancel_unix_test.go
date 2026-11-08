//go:build !windows

package fetch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFetchCancelKillsSSH(t *testing.T) {
	s := newServer(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	// The "ssh" never answers; its sleep is a grandchild of the shell
	// running GIT_SSH_COMMAND, which a plain kill of the shell would orphan.
	cmd := filepath.Join(dir, "ssh")
	script := fmt.Sprintf("#!/bin/sh\nsleep 1000 &\necho $! > %q.tmp\nmv %q.tmp %q\nwait\n", pidFile, pidFile, pidFile)
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s.g.vars["GIT_SSH_COMMAND"] = cmd
	_, gitDir, db := s.g.target()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Fetch(ctx, s.g.options("host:/repo", gitDir, db, s.ids[4]))
		done <- err
	}()
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; {
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		} else if time.Now().After(deadline) {
			t.Fatal("ssh stand-in did not start")
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch did not return after cancellation")
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ssh child %d survived cancellation", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
