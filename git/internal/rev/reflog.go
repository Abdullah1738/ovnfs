package rev

import (
	"errors"
	"io/fs"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
)

// readReflog returns ref's reflog, newest first; a ref without one is an
// unknown revision, as in git.
func (r *resolver) readReflog(full string) ([]refs.ReflogEntry, error) {
	log, err := r.src.Refs.Reflog(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, unknown("no reflog for %s", full)
		}
		return nil, err
	}
	return log, nil
}

// entryValue is the value git reports for reflog entry i (newest first) of
// ref full: its new value, unless the next newer entry recreated the ref
// (old value null), where git finds the log "unexpectedly ended" and
// reports the ref's current value instead (observed).
func (r *resolver) entryValue(full string, log []refs.ReflogEntry, i int) object.ID {
	if i > 0 && log[i-1].Old.IsZero() {
		if ref, err := r.src.Refs.Resolve(full); err == nil {
			return ref.ID
		}
		return log[i-1].New
	}
	return log[i].New
}

// reflogNth implements <ref>@{N}. Observed in git: @{0} is the ref's
// current value (once a reflog exists); @{N} is the value of the N-th
// newest entry, and one past the oldest entry is that entry's old value
// unless the ref was created there.
func (r *resolver) reflogNth(full string, n int) (object.ID, error) {
	log, err := r.readReflog(full)
	if err != nil {
		return object.ID{}, err
	}
	if n == 0 {
		ref, err := r.src.Refs.Resolve(full)
		if err == nil {
			return ref.ID, nil
		}
		if len(log) == 0 {
			return object.ID{}, unknown("log for '%s' is empty", short(full))
		}
		return log[0].New, nil
	}
	switch {
	case n < len(log):
		return r.entryValue(full, log, n), nil
	case n == len(log) && !log[n-1].Old.IsZero():
		return log[n-1].Old, nil
	}
	return object.ID{}, unknown("log for '%s' only has %d entries", short(full), len(log))
}

// reflogAt implements <ref>@{<date>}: the value recorded by the newest
// entry made at or before t. Before the oldest entry git warns and answers
// with that entry's old value, or its new value if the ref was created there.
func (r *resolver) reflogAt(full string, t time.Time) (object.ID, error) {
	log, err := r.readReflog(full)
	if err != nil {
		return object.ID{}, err
	}
	if len(log) == 0 {
		return object.ID{}, unknown("log for '%s' is empty", short(full))
	}
	for i, e := range log {
		if !e.Committer.When.After(t) {
			return r.entryValue(full, log, i), nil
		}
	}
	oldest := log[len(log)-1]
	if !oldest.Old.IsZero() {
		return oldest.Old, nil
	}
	return oldest.New, nil
}

// short is the ref name git uses in reflog messages ("main", "HEAD").
func short(full string) string {
	if s, ok := strings.CutPrefix(full, "refs/heads/"); ok {
		return s
	}
	return full
}

// previousCheckout returns what the n-th previous checkout moved away
// from, read from HEAD's reflog "checkout: moving from <a> to <b>" entries:
// a branch name or an object name for a detached HEAD.
func (r *resolver) previousCheckout(n int) (string, error) {
	log, err := r.readReflog("HEAD")
	if err != nil {
		return "", err
	}
	found := 0
	for _, e := range log {
		rest, ok := strings.CutPrefix(e.Message, "checkout: moving from ")
		if !ok {
			continue
		}
		i := strings.LastIndex(rest, " to ")
		if i < 0 {
			continue
		}
		if found++; found == n {
			return rest[:i], nil
		}
	}
	return "", unknown("@{-%d}: only %d checkouts in HEAD's reflog", n, found)
}
