package checkout

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestStateHolderRequiresLiveLock(t *testing.T) {
	state := t.TempDir()
	pid, held, err := StateHolder(state)
	if err != nil || held || pid != 0 {
		t.Fatalf("missing: %d %t %v", pid, held, err)
	}
	childPID, release := holdStateInChild(t, state, nil)
	defer release()
	pid, held, err = StateHolder(state)
	if err != nil || !held || pid != childPID {
		t.Fatalf("held: %d %t %v", pid, held, err)
	}
	release()
	pid, held, err = StateHolder(state)
	if err != nil || held || pid != 0 {
		t.Fatalf("stale pid: %d %t %v", pid, held, err)
	}
}

func TestStateHolderLegacyAndInvalidMetadata(t *testing.T) {
	for _, value := range []string{"", "garbage", "0\n", "-12\n"} {
		t.Run(value, func(t *testing.T) {
			state := t.TempDir()
			childPID, release := holdStateInChild(t, state, &value)
			defer release()
			pid, held, err := StateHolder(state)
			if err != nil || !held || pid != 0 && pid != childPID {
				t.Fatalf("unknown holder: %d %t %v", pid, held, err)
			}
		})
	}
}

func TestServingIsSpecificToLockGeneration(t *testing.T) {
	state := t.TempDir()
	f, err := lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if Serving(state, os.Getpid()) {
		t.Fatal("cold lock reported serving")
	}
	if err := MarkServing(state); err != nil {
		t.Fatal(err)
	}
	if !Serving(state, os.Getpid()) {
		t.Fatal("current generation not serving")
	}
	f.Close()
	f, err = lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if Serving(state, os.Getpid()) {
		t.Fatal("stale readiness accepted after same-pid restart")
	}
}

func TestStateLockProcess(t *testing.T) {
	state := os.Getenv("OVNFS_TEST_LOCK_STATE")
	if state == "" {
		return
	}
	f, err := lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if os.Getenv("OVNFS_TEST_METADATA_SET") == "1" {
		data := []byte(os.Getenv("OVNFS_TEST_METADATA"))
		if _, err := f.WriteAt(data, 16); err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(int64(16 + len(data))); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("locked")
	io.Copy(io.Discard, os.Stdin)
}

func holdStateInChild(t *testing.T, state string, metadata *string) (int, func()) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestStateLockProcess$")
	cmd.Env = append(os.Environ(), "OVNFS_TEST_LOCK_STATE="+state)
	if metadata != nil {
		cmd.Env = append(cmd.Env, "OVNFS_TEST_METADATA_SET=1", "OVNFS_TEST_METADATA="+*metadata)
	}
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
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("holder: %q %v", line, err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			in.Close()
			if err := cmd.Wait(); err != nil {
				t.Errorf("holder exit: %v", err)
			}
		})
	}
	t.Cleanup(release)
	return cmd.Process.Pid, release
}

func TestStateObserversDoNotExcludeWriter(t *testing.T) {
	state := t.TempDir()
	// A stale lock file must remain observable without taking its lock.
	f, err := lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		for range 1000 {
			if _, _, err := StateHolder(state); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	close(start)
	for range 1000 {
		f, err := lockState(state)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStateHolderIgnoresForeignOpenOnlyObserver(t *testing.T) {
	state := t.TempDir()
	f, err := lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestStateOpenOnlyProcess$")
	cmd.Env = append(os.Environ(), "OVNFS_TEST_OPEN_ONLY_STATE="+state)
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
			t.Errorf("observer: %v", err)
		}
	}()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "open\n" {
		t.Fatalf("observer: %q %v", line, err)
	}
	if pid, held, err := StateHolder(state); err != nil || held || pid != 0 {
		t.Fatalf("open-only observer became holder: %d %t %v", pid, held, err)
	}
	lease, err := lockState(state)
	if err != nil {
		t.Fatalf("observer excluded new writer: %v", err)
	}
	lease.Close()
}

func TestStateOpenOnlyProcess(t *testing.T) {
	state := os.Getenv("OVNFS_TEST_OPEN_ONLY_STATE")
	if state == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(state, "state.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Println("open")
	io.Copy(io.Discard, os.Stdin)
}
