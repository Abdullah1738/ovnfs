package fetch

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// ErrShallowLocked reports that another process holds <commonDir>/shallow.lock.
var ErrShallowLocked = errors.New("fetch: shallow file is locked")

// ReadShallow returns the commits listed in <commonDir>/shallow, the
// repository's shallow boundary; a missing file is an empty boundary.
func ReadShallow(commonDir string) ([]object.ID, error) {
	data, err := os.ReadFile(filepath.Join(commonDir, "shallow"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseShallow(data)
}

func parseShallow(data []byte) ([]object.ID, error) {
	var ids []object.ID
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		id, err := object.ParseID(line)
		if err != nil {
			return nil, fmt.Errorf("fetch: bad shallow line %q: %w", line, err)
		}
		ids = append(ids, id)
	}
	return ids, sc.Err()
}

// UpdateShallow adds and removes commits from <commonDir>/shallow under
// git's lock protocol (shallow.lock created exclusively, then renamed over
// the file). Lines are written sorted, as git writes them; a resulting empty
// boundary removes the file. A held lock is an error matching ErrShallowLocked.
func UpdateShallow(commonDir string, add, remove []object.ID) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	target := filepath.Join(commonDir, "shallow")
	lockPath := target + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: %s exists", ErrShallowLocked, lockPath)
	}
	if err != nil {
		return err
	}
	// Read under the lock so a concurrent writer's update is not lost.
	current, err := ReadShallow(commonDir)
	if err == nil {
		err = writeShallowLock(lock, applyShallow(current, add, remove), target)
	}
	if err != nil {
		lock.Close()
		os.Remove(lockPath)
	}
	return err
}

// applyShallow returns current plus add minus remove, sorted.
func applyShallow(current, add, remove []object.ID) []object.ID {
	set := make(map[object.ID]struct{}, len(current)+len(add))
	for _, id := range current {
		set[id] = struct{}{}
	}
	for _, id := range add {
		set[id] = struct{}{}
	}
	for _, id := range remove {
		delete(set, id)
	}
	ids := make([]object.ID, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, object.ID.Compare)
	return ids
}

// writeShallowLock publishes ids through the open lock file, which it closes
// (Windows cannot rename or delete an open file).
func writeShallowLock(lock *os.File, ids []object.ID, target string) error {
	lockPath := lock.Name()
	if len(ids) == 0 {
		if err := lock.Close(); err != nil {
			return err
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.Remove(lockPath)
	}
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(id.String())
		b.WriteByte('\n')
	}
	_, err := lock.WriteString(b.String())
	if cerr := lock.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(lockPath, target)
}
