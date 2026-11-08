package refs

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// reftableBackend stores refs in the reftable stack of each git directory:
// <dir>/reftable/tables.list naming the tables oldest first.
type reftableBackend struct{}

// loadStackAttempts bounds retries when a table named by tables.list
// vanishes because a concurrent compaction replaced it.
const loadStackAttempts = 5

// loadStack reads the stack in gitDir, oldest table first. A missing
// tables.list is an empty stack.
func loadStack(gitDir string, format object.Format) ([]*table, error) {
	dir := filepath.Join(gitDir, "reftable")
	var lastErr error
	for range loadStackAttempts {
		names, err := readTablesList(dir)
		if err != nil {
			return nil, err
		}
		tables, err := readTables(dir, names, format)
		if !errors.Is(err, fs.ErrNotExist) {
			return tables, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// readTablesList returns the table names listed in dir/tables.list.
func readTablesList(dir string) ([]string, error) {
	data, err := readFile(filepath.Join(dir, "tables.list"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for line := range bytes.Lines(data) {
		if name := strings.TrimSpace(string(line)); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

func readTables(dir string, names []string, format object.Format) ([]*table, error) {
	tables := make([]*table, 0, len(names))
	for _, name := range names {
		if strings.ContainsAny(name, `/\`) {
			return nil, fmt.Errorf("refs: tables.list names %q outside the stack", name)
		}
		data, err := readFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		t, err := parseTable(data, format)
		if err != nil {
			return nil, fmt.Errorf("refs: reftable %s: %w", name, err)
		}
		tables = append(tables, t)
	}
	return tables, nil
}

// lookupStack finds name in tables, newest first; a tombstone hides older
// values.
func lookupStack(tables []*table, name string) (Ref, error) {
	for _, t := range slices.Backward(tables) {
		rec, found, err := t.lookupRef(name)
		if err != nil {
			return Ref{}, err
		}
		if found {
			if rec.valueType == refValDeletion {
				break
			}
			return rec.ref(), nil
		}
	}
	return Ref{}, notExist(name)
}

// stackLogs merges the reflog of name across tables, newest entry first; a
// deletion record hides the entry it shadows.
func stackLogs(tables []*table, name string) ([]logRecord, error) {
	byIndex := map[uint64]logRecord{}
	for _, t := range tables {
		recs, err := t.logs(name, false)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if r.deleted {
				delete(byIndex, r.updateIndex)
			} else {
				byIndex[r.updateIndex] = r
			}
		}
	}
	out := make([]logRecord, 0, len(byIndex))
	for _, r := range byIndex {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b logRecord) int { return cmp.Compare(b.updateIndex, a.updateIndex) })
	return out, nil
}

func (reftableBackend) read(loc location, name string) (Ref, error) {
	tables, err := loadStack(loc.dir, loc.format)
	if err != nil {
		return Ref{}, err
	}
	return lookupStack(tables, name)
}

func (reftableBackend) list(loc location, prefix string, keep func(string) bool) ([]Ref, error) {
	tables, err := loadStack(loc.dir, loc.format)
	if err != nil {
		return nil, err
	}
	merged := map[string]refRecord{}
	for _, t := range tables {
		recs, err := t.refs()
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if strings.HasPrefix(r.name, "refs/") && strings.HasPrefix(r.name, prefix) {
				merged[r.name] = r
			}
		}
	}
	out := make([]Ref, 0, len(merged))
	for name, r := range merged {
		if r.valueType != refValDeletion && keep(name) {
			out = append(out, r.ref())
		}
	}
	slices.SortFunc(out, func(a, b Ref) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (reftableBackend) reflog(loc location, name string) ([]ReflogEntry, error) {
	tables, err := loadStack(loc.dir, loc.format)
	if err != nil {
		return nil, err
	}
	recs, err := stackLogs(tables, name)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("refs: no reflog for %s: %w", name, os.ErrNotExist)
	}
	out := make([]ReflogEntry, len(recs))
	for i, r := range recs {
		out[i] = r.entry
	}
	return out, nil
}

// lockedStack is a stack whose tables.list.lock this process holds, loaded
// under the lock so it cannot change underneath.
type lockedStack struct {
	dir    string // the reftable directory
	lock   *lockFile
	names  []string
	tables []*table
	refs   []refRecord
	logs   []logRecord
}

// lockStack takes gitDir's tables.list.lock (ErrLocked if held) and loads
// the stack.
func lockStack(gitDir string, format object.Format) (*lockedStack, error) {
	dir := filepath.Join(gitDir, "reftable")
	lock, err := createLock(filepath.Join(dir, "tables.list"), lockOptions{})
	if err != nil {
		return nil, err
	}
	st := &lockedStack{dir: dir, lock: lock}
	if st.names, err = readTablesList(dir); err == nil {
		st.tables, err = readTables(dir, st.names, format)
	}
	if err != nil {
		lock.rollback()
		return nil, err
	}
	return st, nil
}

// nextUpdateIndex is the update index of a table appended to the stack.
func (st *lockedStack) nextUpdateIndex() uint64 {
	if len(st.tables) == 0 {
		return 1
	}
	return st.tables[len(st.tables)-1].maxIndex + 1
}

// commit writes the queued records as a new table and appends it to the
// stack (technical/reftable "Update transactions"), releasing the lock.
func (st *lockedStack) commit(format object.Format) error {
	defer st.lock.rollback()
	if len(st.refs) == 0 && len(st.logs) == 0 {
		return nil
	}
	ui := st.nextUpdateIndex()
	for i := range st.refs {
		st.refs[i].updateIndex = ui
	}
	for i := range st.logs {
		st.logs[i].updateIndex = ui
	}
	slices.SortFunc(st.refs, func(a, b refRecord) int { return strings.Compare(a.name, b.name) })
	slices.SortFunc(st.logs, func(a, b logRecord) int { return strings.Compare(a.name, b.name) })
	data, err := tableWriter{format: format, updateIndex: ui}.write(st.refs, st.logs)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(st.dir, "tmp_*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	name := fmt.Sprintf("0x%012x-0x%012x-%08x.ref", ui, ui, rand.Uint32())
	if err := os.Rename(tmp.Name(), filepath.Join(st.dir, name)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	var list []byte
	for _, n := range append(st.names, name) {
		list = append(append(list, n...), '\n')
	}
	err = st.lock.write(list)
	if err == nil {
		err = st.lock.commit()
	}
	if err != nil {
		os.Remove(filepath.Join(st.dir, name))
	}
	return err
}

// hasLog reports whether name has reflog entries in the locked stack.
func (st *lockedStack) hasLog(name string) (bool, error) {
	recs, err := stackLogs(st.tables, name)
	return len(recs) > 0, err
}

func (reftableBackend) commit(s *Store, tx *transaction) error {
	// Lock every stack involved, in a fixed order so concurrent writers
	// touching the same pair cannot deadlock.
	dirs := []string{tx.target.loc.dir}
	for _, l := range tx.logs {
		dirs = append(dirs, l.loc.dir)
	}
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)
	stacks := map[string]*lockedStack{}
	defer func() {
		for _, st := range stacks {
			st.lock.rollback()
		}
	}()
	for _, dir := range dirs {
		st, err := lockStack(dir, s.format)
		if err != nil {
			return err
		}
		stacks[dir] = st
	}
	targetStack := stacks[tx.target.loc.dir]
	current, err := lookupStack(targetStack.tables, tx.target.name)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if current.Symbolic() {
		return fmt.Errorf("refs: %s became a symbolic reference during the update", tx.target.visible)
	}
	if err := tx.check(exists, current.ID); err != nil {
		return err
	}
	noop := exists && current.ID == tx.newID
	if !exists {
		if err := s.checkNameConflict(tx.target.visible); err != nil {
			return err
		}
	}
	if !noop {
		rec := refRecord{name: tx.target.name, valueType: refValOne, value: tx.newID}
		if !tx.peeled.IsZero() {
			rec.valueType, rec.peeled = refValTwo, tx.peeled
		}
		targetStack.refs = append(targetStack.refs, rec)
	}
	if tx.reflog != nil {
		oldID := s.format.Zero()
		if exists {
			oldID = current.ID
		}
		entry := tx.logEntry(oldID)
		for _, l := range tx.loggedRefs(noop) {
			st := stacks[l.loc.dir]
			has, err := st.hasLog(l.name)
			if err != nil {
				return err
			}
			if has || tx.reflog.Policy.creates(l.name) {
				st.logs = append(st.logs, logRecord{name: l.name, entry: entry})
			}
		}
	}
	// The target's stack goes last: once its table is in, the update is
	// visible, and it should not be visible without its reflog.
	for _, dir := range dirs {
		if dir == tx.target.loc.dir {
			continue
		}
		if err := stacks[dir].commit(s.format); err != nil {
			return err
		}
	}
	return targetStack.commit(s.format)
}
