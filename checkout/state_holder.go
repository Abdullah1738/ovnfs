package checkout

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type stateLease struct {
	*os.File
	proof    *os.File
	identity os.FileInfo
}

// POSIX locks disappear when their process closes any descriptor of that
// file. Local queries must use the live lease without reopening its proof;
// serialize that shortcut with lease creation and Close, including aliases.
var stateOwnersMu sync.Mutex
var stateOwners = map[*stateLease]struct{}{}

func (l *stateLease) Close() error {
	stateOwnersMu.Lock()
	defer stateOwnersMu.Unlock()
	delete(stateOwners, l)
	if l.proof != nil {
		_ = l.proof.Close()
	}
	return l.File.Close()
}

func lockState(state string) (*stateLease, error) {
	stateOwnersMu.Lock()
	defer stateOwnersMu.Unlock()
	f, err := os.OpenFile(filepath.Join(state, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, coded(err)
	}
	locked, err := tryStateLock(f)
	if err != nil || !locked {
		f.Close()
		if err != nil {
			return nil, coded(err)
		}
		return nil, failure("exists", "state directory is already open: %s", state)
	}
	proof, err := lockProof(state)
	if err != nil {
		f.Close()
		return nil, coded(err)
	}
	// PID text is diagnostic; only a kernel-held owner record can authorize recovery.
	data := []byte(strconv.Itoa(os.Getpid()) + " " + rand.Text() + "\n")
	if _, err = f.WriteAt(data, 16); err == nil {
		err = f.Truncate(int64(16 + len(data)))
	}
	if err != nil {
		if proof != nil {
			proof.Close()
		}
		f.Close()
		return nil, coded(err)
	}
	identity, err := f.Stat()
	if err != nil {
		if proof != nil {
			proof.Close()
		}
		f.Close()
		return nil, coded(err)
	}
	lease := &stateLease{File: f, proof: proof, identity: identity}
	stateOwners[lease] = struct{}{}
	return lease, nil
}

// StateHolder reports a held state lock even before the admin listener exists.
// pid is zero for a daemon predating kernel owner records; callers must not
// infer that an unknown holder is dead or replace it.
func StateHolder(state string) (pid int, held bool, err error) {
	f, err := os.OpenFile(filepath.Join(state, "state.lock"), os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	stateOwnersMu.Lock()
	defer stateOwnersMu.Unlock()
	identity, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	for owner := range stateOwners {
		if os.SameFile(identity, owner.identity) {
			return os.Getpid(), true, nil
		}
	}
	kernelPID, held, err := stateLockInfo(f)
	if err != nil || !held {
		return 0, held, err
	}
	if kernelPID > 0 {
		return kernelPID, true, nil
	}
	pid, err = proofOwner(state)
	return pid, true, err
}

func lockIdentity(f *os.File) []byte {
	var data [128]byte
	n, _ := f.ReadAt(data[:], 16)
	return bytes.TrimSpace(data[:n])
}

// MarkServing ties readiness to this lock generation, not a socket pathname
// that may have survived a crash or an earlier use of the same pid.
func MarkServing(state string) error {
	f, err := os.Open(filepath.Join(state, "state.lock"))
	if err != nil {
		return err
	}
	defer f.Close()
	return os.WriteFile(filepath.Join(state, "state.ready"), lockIdentity(f), 0600)
}

func Serving(state string, pid int) bool {
	f, err := os.Open(filepath.Join(state, "state.lock"))
	if err != nil {
		return false
	}
	defer f.Close()
	identity := lockIdentity(f)
	fields := strings.Fields(string(identity))
	if len(fields) != 2 || fields[0] != strconv.Itoa(pid) {
		return false
	}
	ready, err := os.ReadFile(filepath.Join(state, "state.ready"))
	return err == nil && bytes.Equal(identity, ready)
}
