package refs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// UpdateOptions configures Update.
type UpdateOptions struct {
	// Old is the value the reference must have before the update; a zero ID
	// requires that it does not exist. Ignored when AnyOld is set.
	Old object.ID
	// AnyOld skips the compare-and-swap check, like git update-ref without
	// <old-oid>.
	AnyOld bool
	// Peeled, for an annotated tag, is the object the tag peels to. The
	// reftable backend records it as git does; loose refs cannot.
	Peeled object.ID
	// Reflog, when set, records the update in the reflogs that git would
	// write; nil writes no reflog at all.
	Reflog *ReflogUpdate
}

// ReflogUpdate is the reflog entry an update records.
type ReflogUpdate struct {
	// Committer identifies who made the update and when.
	Committer object.Signature
	// Message is the reason; git collapses whitespace runs to single spaces.
	Message string
	// Policy decides which missing reflogs are created.
	Policy LogPolicy
}

// LogPolicy mirrors core.logAllRefUpdates: an existing reflog is always
// appended to; the policy decides which missing ones are created.
type LogPolicy int

const (
	// LogExisting creates no reflog (core.logAllRefUpdates=false, git's
	// default in bare repositories).
	LogExisting LogPolicy = iota
	// LogDefault creates reflogs for HEAD and refs under refs/heads/,
	// refs/remotes/ and refs/notes/ (core.logAllRefUpdates=true).
	LogDefault
	// LogAlways creates a reflog for every ref (core.logAllRefUpdates=always).
	LogAlways
)

// creates reports whether p creates a missing reflog for name.
func (p LogPolicy) creates(name string) bool {
	switch p {
	case LogAlways:
		return true
	case LogDefault:
		return name == "HEAD" || strings.HasPrefix(name, "refs/heads/") ||
			strings.HasPrefix(name, "refs/remotes/") || strings.HasPrefix(name, "refs/notes/")
	}
	return false
}

// refTarget is a routed reference name.
type refTarget struct {
	visible string // as the caller spells it
	loc     location
	name    string // within loc
}

// transaction is one reference update after symbolic references have been
// followed.
type transaction struct {
	target refTarget
	// logs are the reflogs that may record the update: the target's first,
	// then the symbolic references leading to it (and HEAD when it points at
	// the target), which the files backend also locks, as git does.
	logs          []refTarget
	newID, peeled object.ID
	old           object.ID
	anyOld        bool
	reflog        *ReflogUpdate
}

// Update sets the reference name to newID with git update-ref <ref> <new>
// <old> semantics: symbolic references are followed and the reference they
// end at is updated (created if missing); unless opts.AnyOld is set, the
// current value must equal opts.Old (a zero Old: must not exist), else the
// error wraps ErrMismatch. A lock held by another writer yields ErrLocked.
// With opts.Reflog the update is appended to the target's reflog and to those
// of the symbolic references followed — and to HEAD's when HEAD points at the
// target — each when that reflog exists or the policy creates it. An update
// that would not change the value leaves the target and its reflog alone but,
// like git, still logs to those symbolic references.
func (s *Store) Update(name string, newID object.ID, opts UpdateOptions) error {
	if newID.Format() != s.format || newID.IsZero() {
		return fmt.Errorf("refs: update %s: invalid new value %q", name, newID)
	}
	if !opts.AnyOld && !opts.Old.IsZero() && opts.Old.Format() != s.format {
		return fmt.Errorf("refs: update %s: invalid old value %q", name, opts.Old)
	}
	var chain []string
	target := name
	for reads := 0; ; reads++ {
		if reads == maxRefReads {
			return fmt.Errorf("refs: %s: %w", name, ErrSymrefDepth)
		}
		r, err := s.Read(target)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		if !r.Symbolic() {
			break
		}
		chain = append(chain, target)
		target = r.Target
	}
	if isSpecialPseudoref(target) {
		return fmt.Errorf("refs: refusing to update pseudoref %s", target)
	}
	if target != "HEAD" && !slices.Contains(chain, "HEAD") && s.headPointsAt(target) {
		chain = append(chain, "HEAD")
	}
	tx := &transaction{newID: newID, peeled: opts.Peeled, old: opts.Old, anyOld: opts.AnyOld, reflog: opts.Reflog}
	for _, n := range append([]string{target}, chain...) {
		loc, inner, err := s.route(n)
		if err != nil {
			return err
		}
		tx.logs = append(tx.logs, refTarget{visible: n, loc: loc, name: inner})
	}
	tx.target = tx.logs[0]
	if tx.reflog != nil {
		r := *tx.reflog
		r.Message = normalizeLogMessage(r.Message)
		tx.reflog = &r
	}
	return s.backend.commit(s, tx)
}

// headPointsAt reports whether this worktree's HEAD is a symbolic reference
// that ends at name.
func (s *Store) headPointsAt(name string) bool {
	end, err := s.follow("HEAD")
	if err != nil {
		return false
	}
	if end.Symbolic() {
		return end.Target == name
	}
	return end.Name == name && end.Name != "HEAD"
}

// check applies the compare-and-swap condition to the current value.
func (tx *transaction) check(exists bool, current object.ID) error {
	switch {
	case tx.anyOld:
		return nil
	case tx.old.IsZero() && exists:
		return fmt.Errorf("refs: %s already exists at %s: %w", tx.target.visible, current, ErrMismatch)
	case !tx.old.IsZero() && !exists:
		return fmt.Errorf("refs: %s does not exist, expected %s: %w", tx.target.visible, tx.old, ErrMismatch)
	case !tx.old.IsZero() && current != tx.old:
		return fmt.Errorf("refs: %s is at %s, expected %s: %w", tx.target.visible, current, tx.old, ErrMismatch)
	}
	return nil
}

// loggedRefs returns the refs whose reflogs record the update. When the
// value does not change (noop), git leaves the target's reflog alone but
// still logs the symbolic refs it went through, HEAD included.
func (tx *transaction) loggedRefs(noop bool) []refTarget {
	if noop {
		return tx.logs[1:]
	}
	return tx.logs
}

func (tx *transaction) logEntry(oldID object.ID) ReflogEntry {
	return ReflogEntry{Old: oldID, New: tx.newID, Committer: tx.reflog.Committer, Message: tx.reflog.Message}
}

// normalizeLogMessage collapses whitespace runs, newlines included, into
// single spaces and trims the ends, as git does before storing a reflog
// message: a newline would otherwise end the log line.
func normalizeLogMessage(msg string) string {
	return strings.Join(strings.FieldsFunc(msg, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			return true
		}
		return false
	}), " ")
}

// checkNameConflict rejects creating name when an existing reference is a
// path prefix of it ("refs/heads/a" blocks "refs/heads/a/b") or lives below
// it: the files backend cannot store both, and git refuses them in every
// backend.
func (s *Store) checkNameConflict(name string) error {
	for i := range len(name) {
		if name[i] != '/' {
			continue
		}
		if _, err := s.Read(name[:i]); err == nil {
			return fmt.Errorf("refs: cannot create %s: %s exists", name, name[:i])
		}
	}
	below, err := s.List(name + "/")
	if err != nil {
		return err
	}
	if len(below) > 0 {
		return fmt.Errorf("refs: cannot create %s: %s exists", name, below[0].Name)
	}
	return nil
}

func (filesBackend) commit(s *Store, tx *transaction) error {
	path := func(t refTarget) string { return filepath.Join(t.loc.dir, filepath.FromSlash(t.name)) }
	if _, err := (filesBackend{}).read(tx.target.loc, tx.target.name); errors.Is(err, fs.ErrNotExist) {
		// Checked before locking so a conflicting file does not get in the
		// way of creating the lock's directory.
		if err := s.checkNameConflict(tx.target.visible); err != nil {
			return err
		}
	}
	targetPath := path(tx.target)
	lock, err := createLock(targetPath, lockOptions{mkdirs: true})
	if err != nil {
		return err
	}
	defer lock.rollback()
	for _, t := range tx.logs[1:] {
		l, err := createLock(path(t), lockOptions{})
		if err != nil {
			return err
		}
		defer l.rollback()
	}
	current, err := (filesBackend{}).read(tx.target.loc, tx.target.name)
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
	if !noop {
		if err := lock.write([]byte(tx.newID.String() + "\n")); err != nil {
			return err
		}
	}
	if tx.reflog != nil {
		oldID := s.format.Zero()
		if exists {
			oldID = current.ID
		}
		line := formatReflogLine(tx.logEntry(oldID))
		for _, t := range tx.loggedRefs(noop) {
			logPath := filepath.Join(t.loc.dir, "logs", filepath.FromSlash(t.name))
			if err := appendReflog(logPath, line, tx.reflog.Policy.creates(t.name)); err != nil {
				return err
			}
		}
	}
	if noop {
		return nil
	}
	if isDir(targetPath) {
		// An empty directory left behind by deleted refs below this name.
		os.Remove(targetPath)
	}
	return lock.commit()
}

// formatReflogLine is a logs/ file line: "<old> <new> <signature>\t<message>\n",
// without the tab when there is no message.
func formatReflogLine(e ReflogEntry) []byte {
	b := []byte(e.Old.String() + " " + e.New.String() + " ")
	b = e.Committer.Append(b)
	if e.Message != "" {
		b = append(append(b, '\t'), e.Message...)
	}
	return append(b, '\n')
}

// appendReflog appends line to the log file at path, creating it (and its
// directories) only when create is set.
func appendReflog(path string, line []byte, create bool) error {
	flags := os.O_WRONLY | os.O_APPEND
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return err
		}
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0o666)
	if err != nil {
		if !create && (errors.Is(err, fs.ErrNotExist) || isNotDir(err)) {
			return nil
		}
		return err
	}
	_, err = f.Write(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
