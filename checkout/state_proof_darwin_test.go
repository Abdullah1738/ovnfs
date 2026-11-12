//go:build darwin

package checkout

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLocalObservationPreservesKernelOwner(t *testing.T) {
	state := t.TempDir()
	lease, err := lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(state, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{state, alias} {
		pid, held, err := StateHolder(path)
		if err != nil || !held || pid != os.Getpid() {
			t.Fatalf("local observer: %d %t %v", pid, held, err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestQueryKernelOwnerProcess$")
	cmd.Env = append(os.Environ(), "OVNFS_TEST_QUERY_STATE="+state, "OVNFS_TEST_QUERY_PID="+strconv.Itoa(os.Getpid()))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("owner proof lost: %s %v", out, err)
	}
}

func TestQueryKernelOwnerProcess(t *testing.T) {
	state := os.Getenv("OVNFS_TEST_QUERY_STATE")
	if state == "" {
		return
	}
	want, _ := strconv.Atoi(os.Getenv("OVNFS_TEST_QUERY_PID"))
	pid, held, err := StateHolder(state)
	if err != nil || !held || pid != want {
		t.Fatalf("kernel owner: %d %t %v want %d", pid, held, err, want)
	}
}

func TestLegacyFlockNeverTrustsStoredPID(t *testing.T) {
	state := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLegacyFlockProcess$")
	cmd.Env = append(os.Environ(), "OVNFS_TEST_LEGACY_STATE="+state)
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
			t.Errorf("legacy holder: %v", err)
		}
	}()
	var ready [1]byte
	if _, err := io.ReadFull(out, ready[:]); err != nil {
		t.Fatal(err)
	}
	pid, held, err := StateHolder(state)
	if err != nil || !held || pid != 0 {
		t.Fatalf("trusted legacy PID text: %d %t %v", pid, held, err)
	}
}

func TestLegacyFlockProcess(t *testing.T) {
	state := os.Getenv("OVNFS_TEST_LEGACY_STATE")
	if state == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(state, "state.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if ok, err := tryStateLock(f); err != nil || !ok {
		t.Fatalf("legacy lock: %t %v", ok, err)
	}
	if _, err := f.WriteAt([]byte(fmt.Sprintf("%d stale-generation", os.Getpid())), 16); err != nil {
		t.Fatal(err)
	}
	fmt.Print("R")
	io.Copy(io.Discard, os.Stdin)
}
