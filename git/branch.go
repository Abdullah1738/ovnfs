package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// writeStamped writes data to name and gives it modification time stamp.
func writeStamped(name string, data []byte, stamp time.Time) error {
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return err
	}
	return os.Chtimes(name, stamp, stamp)
}

// ErrBranchConflict identifies a branch or index that cannot be advanced safely.
var ErrBranchConflict = errors.New("branch conflict")

// ErrBranchDryRun preserves the unsupported dry run for branches not checked out.
var ErrBranchDryRun = errors.New("--branch has no dry run")

// BranchChange describes an index entry advanced with the checked-out branch.
type BranchChange struct {
	Path, Status string
}

type branchEntry struct {
	mode, oid string
}

type branchUpdate struct {
	BranchChange
	old, next branchEntry
}

// FetchBranch fetches a checkout's commits, advancing only the index when branch is checked out.
func (r *Repo) FetchBranch(ctx context.Context, gitdir, branch string, dryRun bool) (string, []BranchChange, error) {
	if _, err := r.text(ctx, "check-ref-format", "--branch", branch); err != nil {
		return "", nil, err
	}
	ref := "refs/heads/" + branch
	head, err := r.optional(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", nil, err
	}
	if head != ref {
		if dryRun {
			return "", nil, ErrBranchDryRun
		}
		if _, err := r.text(ctx, "fetch", "--no-tags", "--quiet", gitdir, "HEAD:"+ref); err != nil {
			return "", nil, err
		}
		commit, err := r.text(ctx, "rev-parse", "--verify", ref)
		return commit, nil, err
	}
	// Keep the lock until the ref and index agree, including a failed ref CAS.
	// The replacement index has its own temporary name so the lock stays held
	// across both its atomic installation and any rollback.
	if !dryRun {
		lock, err := os.OpenFile(r.IndexPath()+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", nil, err
		}
		defer os.Remove(lock.Name())
		if err := lock.Close(); err != nil {
			return "", nil, err
		}
		head, err = r.optional(ctx, "symbolic-ref", "--quiet", "HEAD")
		if err != nil {
			return "", nil, err
		}
		if head != ref {
			return "", nil, fmt.Errorf("%w: the checked-out branch changed; retry or choose a different branch name", ErrBranchConflict)
		}
	}
	old, err := r.text(ctx, "rev-parse", "--verify", ref)
	if err != nil {
		return "", nil, err
	}
	// --git-dir is explicit; command still strips inherited git environment.
	checkoutText := func(args ...string) (string, error) {
		return r.text(ctx, append([]string{"--git-dir", gitdir}, args...)...)
	}
	commit, err := checkoutText("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", nil, err
	}
	_, err = checkoutText("merge-base", "--is-ancestor", old, commit)
	if err != nil {
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", nil, fmt.Errorf("%w: %s is not an ancestor of the checkout's HEAD; choose a different branch name", ErrBranchConflict, branch)
		}
		return "", nil, err
	}
	raw, err := run(r.command(ctx, "--git-dir", gitdir, "diff-tree", "--no-commit-id", "--raw", "--no-abbrev", "--no-renames", "-r", "-z", old, commit))
	if err != nil {
		return "", nil, err
	}
	var updates []branchUpdate
	for len(raw) > 0 {
		record, rest, ok := bytes.Cut(raw, []byte{0})
		if !ok {
			return "", nil, errors.New("invalid git tree diff")
		}
		path, rest, ok := bytes.Cut(rest, []byte{0})
		fields := strings.Fields(string(record))
		if !ok || len(fields) != 5 {
			return "", nil, errors.New("invalid git tree diff entry")
		}
		updates = append(updates, branchUpdate{
			Path: string(path), Status: fields[4],
			old:  branchEntry{strings.TrimPrefix(fields[0], ":"), fields[2]},
			next: branchEntry{fields[1], fields[3]},
		})
		raw = rest
	}
	index, err := run(r.command(ctx, "ls-files", "--stage", "-z"))
	if err != nil {
		return "", nil, err
	}
	entries := make(map[string][]string)
	for record := range bytes.SplitSeq(index, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		entry, path, ok := strings.Cut(string(record), "\t")
		if !ok {
			return "", nil, errors.New("invalid git index entry")
		}
		entries[path] = append(entries[path], entry)
	}
	var conflicts []string
	changed := make(map[string]bool, len(updates))
	added := make(map[string]bool)
	for _, update := range updates {
		changed[update.Path] = true
		if update.old.mode == "000000" && update.next.mode != "000000" {
			added[update.Path] = true
		}
	}
	changes := make([]BranchChange, 0, len(updates))
	for _, update := range updates {
		staged := entries[update.Path]
		matches := len(staged) == 0 && update.old.mode == "000000"
		if update.old.mode != "000000" {
			matches = len(staged) == 1 && staged[0] == update.old.mode+" "+update.old.oid+" 0"
		}
		if !matches {
			conflicts = append(conflicts, update.Path)
		}
		changes = append(changes, update.BranchChange)
	}
	// A staged file/directory collision is also unrelated staged work, not
	// permission for update-index to silently remove that entry.
	collisions := make(map[string]bool)
	for path := range added {
		for parent := filepath.ToSlash(filepath.Dir(path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			if len(entries[parent]) != 0 && !changed[parent] {
				collisions[parent] = true
			}
		}
	}
	if len(added) != 0 {
		for path := range entries {
			if changed[path] {
				continue
			}
			for parent := filepath.ToSlash(filepath.Dir(path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
				if added[parent] {
					collisions[path] = true
					break
				}
			}
		}
	}
	for path := range collisions {
		conflicts = append(conflicts, path)
	}
	if len(conflicts) != 0 {
		sort.Strings(conflicts)
		return "", nil, fmt.Errorf("%w: staged changes in %s; choose a different branch name", ErrBranchConflict, strings.Join(conflicts, ", "))
	}
	if dryRun || old == commit {
		return commit, changes, nil
	}
	// Fetch objects without touching any source ref or FETCH_HEAD.
	if _, err := r.text(ctx, "fetch", "--no-tags", "--no-write-fetch-head", "--quiet", gitdir, commit); err != nil {
		return "", nil, err
	}
	if len(updates) == 0 {
		_, err := r.text(ctx, "update-ref", ref, commit, old)
		return commit, changes, err
	}
	candidate, err := os.CreateTemp(r.GitDir, "ovnfs-index-*")
	if err != nil {
		return "", nil, err
	}
	candidatePath := candidate.Name()
	defer os.Remove(candidatePath)
	defer os.Remove(candidatePath + ".lock")
	if err := candidate.Close(); err != nil {
		return "", nil, err
	}
	original, err := os.ReadFile(r.IndexPath())
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return "", nil, err
	}
	// Git treats entries modified no earlier than the index file as racily
	// clean and rechecks their content. Copies must keep the original index
	// mtime, or a file edited right after it was staged reads as unchanged.
	var stamp time.Time
	if missing {
		if err := os.Remove(candidatePath); err != nil {
			return "", nil, err
		}
	} else {
		info, err := os.Stat(r.IndexPath())
		if err != nil {
			return "", nil, err
		}
		stamp = info.ModTime()
		if err := writeStamped(candidatePath, original, stamp); err != nil {
			return "", nil, err
		}
	}
	var input bytes.Buffer
	// Remove changed entries first so file/directory replacements do not
	// depend on the lexical ordering of the tree diff.
	for _, update := range updates {
		fmt.Fprintf(&input, "0 %s\t%s%c", strings.Repeat("0", len(update.next.oid)), update.Path, byte(0))
	}
	for _, update := range updates {
		if update.next.mode != "000000" {
			fmt.Fprintf(&input, "%s %s\t%s%c", update.next.mode, update.next.oid, update.Path, byte(0))
		}
	}
	cmd := r.command(ctx, "update-index", "-z", "--index-info")
	cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+candidatePath)
	cmd.Stdin = &input
	if _, err := run(cmd); err != nil {
		return "", nil, err
	}
	backup, err := os.CreateTemp(r.GitDir, "ovnfs-index-backup-*")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(backup.Name())
	if _, err := backup.Write(original); err != nil {
		backup.Close()
		return "", nil, err
	}
	if err := backup.Close(); err != nil {
		return "", nil, err
	}
	if !missing {
		if err := os.Chtimes(backup.Name(), stamp, stamp); err != nil {
			return "", nil, err
		}
	}
	if err := os.Rename(candidatePath, r.IndexPath()); err != nil {
		return "", nil, err
	}
	// Once the index is installed, cancellation must not kill update-ref after
	// it has moved the ref but before its successful exit can be observed.
	var refErr error
	if ctx.Err() != nil {
		refErr = ctx.Err()
	} else {
		_, refErr = r.text(context.WithoutCancel(ctx), "update-ref", ref, commit, old)
	}
	if err := refErr; err != nil {
		var rollback error
		if missing {
			rollback = os.Remove(r.IndexPath())
		} else {
			rollback = os.Rename(backup.Name(), r.IndexPath())
		}
		if rollback != nil {
			return "", nil, fmt.Errorf("%w; restoring index: %v", err, rollback)
		}
		return "", nil, err
	}
	return commit, changes, nil
}
