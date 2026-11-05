// Package git reads facts from git repositories found under a source folder.
//
// Git is never required: a folder without a repository is captured the same
// way. Where a repository exists, it contributes facts (which paths are
// tracked or ignored, which commit is pushed), an object database to read
// unchanged tracked content from, and a private git directory that makes
// `git` work inside a checkout without writing to the user's repository.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gitindex "github.com/go-git/go-git/v5/plumbing/format/index"
)

// ErrNotRepo reports a directory outside a git work tree, or a machine without git.
var ErrNotRepo = errors.New("not a git work tree")

// Repo identifies a work tree and its shared object database.
type Repo struct {
	Top, CommonDir, GitDir string
}

// command builds an isolated git command that never inherits another index, work tree or hooks.
func (r *Repo) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Top, "-c", "core.hooksPath=/dev/null"}, args...)...)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "LC_ALL", "GIT_INDEX_FILE", "GIT_WORK_TREE", "GIT_DIR", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C")
	return cmd
}

// run returns stdout or git's stderr, preserving the original process error.
func run(cmd *exec.Cmd) ([]byte, error) {
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(diagnostic.String())
		if msg == "" {
			msg = err.Error()
		}
		args := cmd.Args[1:]
		for len(args) >= 2 && (args[0] == "-C" || args[0] == "-c") {
			args = args[2:]
		}
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), msg, err)
	}
	return out, nil
}

// text runs a git command and trims its textual result.
func (r *Repo) text(ctx context.Context, args ...string) (string, error) {
	out, err := run(r.command(ctx, args...))
	return strings.TrimSpace(string(out)), err
}

// optional runs a query whose exit status 1 means "no value" rather than failure.
func (r *Repo) optional(ctx context.Context, args ...string) (string, error) {
	value, err := r.text(ctx, args...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	return value, err
}

// Open resolves the work tree containing dir, its common directory and its own git directory.
func Open(dir string) (*Repo, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	r := &Repo{Top: absolute}
	out, err := r.text(context.Background(), "rev-parse", "--path-format=absolute", "--is-inside-work-tree", "--show-toplevel", "--git-common-dir", "--git-dir")
	if err != nil {
		return nil, ErrNotRepo
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 4 || lines[0] != "true" {
		return nil, ErrNotRepo
	}
	r.Top, r.CommonDir, r.GitDir = filepath.Clean(lines[1]), filepath.Clean(lines[2]), filepath.Clean(lines[3])
	return r, nil
}

// IsRoot reports whether dir holds a .git entry, the cheap check made before Open while walking.
func IsRoot(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// IndexEntry is one tracked path with the stat data git cached for it.
type IndexEntry struct {
	Path         string
	OID          string
	Mode         uint32
	Size         uint32
	Mtime, Ctime time.Time
	Inode        uint32
}

// Index is the decoded index of a repository at the moment it was read.
type Index struct {
	Entries  []IndexEntry
	Modified time.Time // the index file's mtime; entries modified at or after it are racily clean
}

// IndexPath is the work tree's live index file.
func (r *Repo) IndexPath() string { return filepath.Join(r.GitDir, "index") }

// ReadIndex decodes an index file without running git. A missing index is empty.
func ReadIndex(name string) (Index, error) {
	file, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return Index{}, nil
	}
	if err != nil {
		return Index{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Index{}, err
	}
	var decoded gitindex.Index
	if err := gitindex.NewDecoder(file).Decode(&decoded); err != nil {
		return Index{}, fmt.Errorf("read %s: %w", name, err)
	}
	out := Index{Entries: make([]IndexEntry, 0, len(decoded.Entries)), Modified: info.ModTime()}
	for _, e := range decoded.Entries {
		// A merged entry has stage 0 on disk; go-git's Merged constant is 1 (AncestorMode).
		if e.Stage != 0 || e.SkipWorktree || e.IntentToAdd {
			continue
		}
		out.Entries = append(out.Entries, IndexEntry{Path: e.Name, OID: e.Hash.String(), Mode: uint32(e.Mode), Size: e.Size, Mtime: e.ModifiedAt, Ctime: e.CreatedAt, Inode: e.Inode})
	}
	return out, nil
}

// Head returns the commit HEAD points at, or "" on an unborn branch.
func (r *Repo) Head(ctx context.Context) (string, error) {
	return r.optional(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
}

// Busy names an unfinished merge, rebase, cherry-pick or revert, whose index cannot be trusted as a base.
func (r *Repo) Busy() string {
	for _, state := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(r.GitDir, state)); err == nil {
			return state
		}
	}
	return ""
}

// Upstream picks the remote-tracking branch a launch elsewhere would fetch from, as
// "<remote>/<branch>": the current branch's upstream, else origin's default branch.
// Both results are "" when the repository has neither.
func (r *Repo) Upstream(ctx context.Context) (ref, commit string, err error) {
	for _, candidate := range []string{"@{upstream}", "origin/HEAD"} {
		// A missing upstream is a fatal error for rev-parse, not a lookup miss.
		name, lookupErr := r.text(ctx, "rev-parse", "--abbrev-ref", candidate)
		if lookupErr != nil || name == "" || name == candidate {
			continue
		}
		commit, err = r.optional(ctx, "rev-parse", "--verify", "--quiet", name+"^{commit}")
		if err != nil || commit != "" {
			return name, commit, err
		}
	}
	return "", "", nil
}

// Remote returns the URL of the remote an Upstream name such as "origin/main" belongs to.
func (r *Repo) Remote(ctx context.Context, upstream string) (string, error) {
	name, _, _ := strings.Cut(upstream, "/")
	if name == "" {
		name = "origin"
	}
	return r.optional(ctx, "config", "--get", "remote."+name+".url")
}

// Contains reports whether commit is reachable from tip.
func (r *Repo) Contains(ctx context.Context, tip, commit string) (bool, error) {
	_, err := r.text(ctx, "merge-base", "--is-ancestor", commit, tip)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// TreeEntry is a recursive ls-tree record; Mode uses git's octal mode encoding.
type TreeEntry struct {
	Mode            uint32
	Type, OID, Path string
	Size            int64
}

// LsTree returns a commit's recursive tree without interpreting quoted filenames.
func (r *Repo) LsTree(ctx context.Context, tree string) ([]TreeEntry, error) {
	return r.lsTree(ctx, tree, false)
}

func (r *Repo) lsTree(ctx context.Context, tree string, sizes bool) ([]TreeEntry, error) {
	args := []string{"ls-tree", "-r", "-z", "--full-tree"}
	if sizes {
		args = append(args, "-l")
	}
	out, err := run(r.command(ctx, append(args, tree)...))
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for raw := range bytes.SplitSeq(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		header, name, ok := strings.Cut(string(raw), "\t")
		fields := strings.Fields(header)
		if !ok || (!sizes && len(fields) != 3) || (sizes && len(fields) != 4) {
			return nil, fmt.Errorf("invalid ls-tree record %q", raw)
		}
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil {
			return nil, err
		}
		var size int64
		if sizes && fields[3] != "-" {
			size, err = strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				return nil, err
			}
		}
		entries = append(entries, TreeEntry{Mode: uint32(mode), Type: fields[1], OID: fields[2], Path: name, Size: size})
	}
	return entries, nil
}

// Labels name the three sides of a merge in the conflict markers it writes.
type Labels struct{ Ours, Base, Theirs string }

// MergeFile merges theirs into ours against base with git merge-file, writing nothing;
// clean reports whether the result has no conflict markers, which carry labels.
func MergeFile(ctx context.Context, ours, base, theirs string, labels Labels) (merged []byte, clean bool, err error) {
	cmd := exec.CommandContext(ctx, "git", "merge-file", "-p", "-L", labels.Ours, "-L", labels.Base, "-L", labels.Theirs, "--", ours, base, theirs)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	merged, err = cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() < 128 {
		return merged, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("git merge-file: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return merged, true, nil
}

// CopyIndex snapshots an index file to dst (clone on APFS), so later reads see the captured state.
func CopyIndex(src, dst string) error { return copyFile(src, dst) }

// copyFile clones bytes on APFS and copies elsewhere, replacing dst.
func copyFile(src, dst string) error {
	if cloned, err := cloneIndexFile(src, dst); cloned || err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
