package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/charmbracelet/colorprofile"
	"github.com/stencil-hq/ovnfs/checkout"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanceledDaemonOperationsHaveNoEffects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state := t.TempDir()
	if _, _, err := spawnDaemon(ctx, state, filepath.Join(state, "mount"), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("spawn: %v", err)
	}
	if err := recoverDaemon(ctx, state, filepath.Join(state, "mount"), os.Getpid()); !errors.Is(err, context.Canceled) {
		t.Fatalf("recovery: %v", err)
	}
}

func TestDaemonHolderRejectsUnrelatedProcess(t *testing.T) {
	state := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestUnrelatedRegistryProcess$")
	cmd.Env = append(os.Environ(), "OVNFS_TEST_UNRELATED_STATE="+state)
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
			t.Errorf("holder: %v", err)
		}
	}()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("holder: %q %v", line, err)
	}
	pid, held, err := daemonHolder(context.Background(), state)
	if err != nil || !held || pid != 0 {
		t.Fatalf("accepted non-serve test process as daemon: %d %t %v", pid, held, err)
	}
	var diagnostic strings.Builder
	previousStderr := stderr
	stderr = &colorprofile.Writer{Forward: &diagnostic}
	defer func() { stderr = previousStderr }()
	c := &cli{ctx: context.Background(), state: state, mount: filepath.Join(state, "mount")}
	if err := (&statusCmd{}).Run(c); err == nil {
		t.Fatal("unverified holder reported ready")
	}
	if text := diagnostic.String(); !strings.Contains(text, "owner PID unavailable") || strings.Contains(text, "pid 0") || strings.Contains(text, "not running") {
		t.Fatalf("misleading owner diagnostic: %s", text)
	}

}

func TestUnrelatedRegistryProcess(t *testing.T) {
	state := os.Getenv("OVNFS_TEST_UNRELATED_STATE")
	if state == "" {
		return
	}
	reg, err := checkout.Open(checkout.Config{State: state, Mount: filepath.Join(state, "mount")})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	fmt.Println("locked")
	io.Copy(io.Discard, os.Stdin)
}
